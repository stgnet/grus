package cmd

import (
	"database/sql"
	"errors"
)

// The AI job queue (plan section 9, "How work flows").
//
// Jobs are rows in each group's file, written by the same command as the
// change that needs the work: CreatePost queues a check and a digest, a new
// comment re-queues the digest and every note written from that thread. So
// work can't be lost between "saved" and "queued", and every node's copy of
// the queue is identical.
//
// A worker claims a job through the leader (ClaimJob) with a lease, runs the
// model against its own local copy, and submits the result as a command
// carrying the version it read. A result for an older version is dropped
// and the job left waiting, so a slow worker never overwrites newer content.

// Job kinds.
const (
	JobCheck  = "check"  // a new or edited post: find older posts on the same topic (M5 adds moderation)
	JobDigest = "digest" // a thread's stored factual summary, which search reads
	JobNote   = "note"   // write or refresh one note
)

// Timing, from the plan: rewrite after activity settles (about 15 minutes of
// quiet), at most once an hour, and never let a busy thread wait forever.
const (
	QuietPeriod = 15 * 60
	MinRerun    = 60 * 60
	MaxWait     = 2 * 60 * 60
	JobLease    = 5 * 60
	MaxAttempts = 5
)

// schedule queues a job, or pushes back one that's already waiting.
//
//   - waiting already: run_after moves to runAfter (debounce), but never past
//     pending_since + MaxWait, so steady activity can't starve it.
//   - done before: it's re-opened, but not sooner than MinRerun after it
//     last ran.
func schedule(tx *sql.Tx, kind string, refID, version, runAfter, at int64) error {
	_, err := tx.Exec(`
		INSERT INTO jobs (kind, ref_id, ref_version, run_after, pending_since, created_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?5)
		ON CONFLICT (kind, ref_id) DO UPDATE SET
		  ref_version   = excluded.ref_version,
		  attempts      = 0,
		  last_error    = NULL,
		  pending_since = CASE WHEN jobs.done_at IS NULL THEN jobs.pending_since ELSE excluded.pending_since END,
		  run_after     = CASE
		    WHEN jobs.done_at IS NULL THEN MIN(excluded.run_after, jobs.pending_since + ?6)
		    ELSE MAX(excluded.run_after, COALESCE(jobs.last_run_at, 0) + ?7) END,
		  done_at       = NULL`,
		kind, refID, version, runAfter, at, MaxWait, MinRerun)
	return err
}

// threadChanged records that something in a thread changed (the post was
// edited, a comment was added, edited, deleted or restored). It bumps the
// thread's version, re-queues its digest, and marks stale every note written
// from this thread that sits on another post, so they're rewritten once
// things settle.
func threadChanged(tx *sql.Tx, groupID, postID, at int64) error {
	// A post that's an update under another post (a continuation) is part
	// of that thread.
	var parent sql.NullInt64
	if err := tx.QueryRow(`SELECT continues_post_id FROM posts WHERE id = ?`, postID).Scan(&parent); err != nil {
		return err
	}
	ids := []int64{postID}
	if parent.Valid {
		ids = append(ids, parent.Int64)
	}
	for _, id := range ids {
		var v int64
		if err := tx.QueryRow(`UPDATE posts SET thread_version = thread_version + 1 WHERE id = ? RETURNING thread_version`, id).Scan(&v); err != nil {
			return err
		}
		if err := schedule(tx, JobDigest, id, v, at+QuietPeriod, at); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT n.id FROM notes n JOIN note_sources s ON s.note_id = n.id
			WHERE s.group_id = ? AND s.post_id = ? AND n.host_post_id != ? AND n.state = 'active'`, groupID, id, id)
		if err != nil {
			return err
		}
		var notes []int64
		for rows.Next() {
			var n int64
			rows.Scan(&n)
			notes = append(notes, n)
		}
		rows.Close()
		for _, n := range notes {
			if _, err := tx.Exec(`UPDATE notes SET stale = 1 WHERE id = ?`, n); err != nil {
				return err
			}
			nv, err := noteVersion(tx, n)
			if err != nil {
				return err
			}
			if err := schedule(tx, JobNote, n, nv, at+QuietPeriod, at); err != nil {
				return err
			}
		}
	}
	return nil
}

// postEdited bumps a post's own version and queues a fresh check.
func postEdited(tx *sql.Tx, groupID, postID, at int64) error {
	var v int64
	if err := tx.QueryRow(`UPDATE posts SET version = version + 1 WHERE id = ? RETURNING version`, postID).Scan(&v); err != nil {
		return err
	}
	if err := schedule(tx, JobCheck, postID, v, at, at); err != nil {
		return err
	}
	return threadChanged(tx, groupID, postID, at)
}

// ClaimJob takes a job for a worker until the lease runs out. It returns
// true if the worker got it; false means another worker has it, or it's no
// longer due (done, or pushed back by new activity).
type ClaimJob struct {
	GroupID int64
	JobID   int64
	Worker  string
	At      int64
}

func (c *ClaimJob) Apply(a *Applier) (any, error) {
	got := false
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE jobs SET claimed_by = ?, lease_until = ?, attempts = attempts + 1
			WHERE id = ? AND done_at IS NULL AND run_after <= ? AND (lease_until IS NULL OR lease_until < ?)`,
			c.Worker, c.At+JobLease, c.JobID, c.At, c.At)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		got = n == 1
		return nil
	})
	return got, err
}

// FailJob records that a worker couldn't finish a job (the model was down,
// or returned nonsense). It's retried with growing gaps, and given up on
// after MaxAttempts; the next change to the content queues it again.
type FailJob struct {
	GroupID int64
	JobID   int64
	Worker  string
	Error   string
	At      int64
}

func (c *FailJob) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var attempts int64
		var claimed sql.NullString
		err := tx.QueryRow(`SELECT attempts, claimed_by FROM jobs WHERE id = ?`, c.JobID).Scan(&attempts, &claimed)
		if errors.Is(err, sql.ErrNoRows) || claimed.String != c.Worker {
			return nil // someone else has it now
		}
		if err != nil {
			return err
		}
		if attempts >= MaxAttempts {
			_, err = tx.Exec(`UPDATE jobs SET claimed_by = NULL, lease_until = NULL, done_at = ?, last_error = ? WHERE id = ?`,
				c.At, c.Error, c.JobID)
			return err
		}
		// 1, 4, 9, 16 minutes: quick enough for a blip, patient with an outage.
		_, err = tx.Exec(`UPDATE jobs SET claimed_by = NULL, lease_until = NULL, run_after = ?, last_error = ? WHERE id = ?`,
			c.At+attempts*attempts*60, c.Error, c.JobID)
		return err
	})
}

// finishJob closes a job whose result is being written. current is the
// version of the content now; version is what the worker read. If they
// differ the result is stale: the job is released (it's already been
// re-queued by the change) and false tells the caller to write nothing.
func finishJob(tx *sql.Tx, jobID int64, worker string, version, current, at int64) (bool, error) {
	var claimed sql.NullString
	err := tx.QueryRow(`SELECT claimed_by FROM jobs WHERE id = ?`, jobID).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if claimed.String != worker {
		return false, nil // the lease ran out and someone else took it
	}
	if version != current {
		_, err := tx.Exec(`UPDATE jobs SET claimed_by = NULL, lease_until = NULL WHERE id = ?`, jobID)
		return false, err
	}
	_, err = tx.Exec(`UPDATE jobs SET claimed_by = NULL, lease_until = NULL, done_at = ?, last_run_at = ?, last_error = NULL
		WHERE id = ?`, at, at, jobID)
	return true, err
}
