package cmd

import (
	"database/sql"
	"errors"
	"strings"
)

// Link notes, digests, and continuations (plan section 2, "Connecting new
// posts to older ones").
//
// A link is a pair of posts on the same subject. Each side gets a note: a
// short text, written by a worker, saying what the *other* post adds from
// this post's point of view. The link is the relationship; the notes are
// what readers see.

// MaxNoteLen caps any system-written text. Notes are a few sentences; a
// model that runs on is cut off rather than trusted.
const MaxNoteLen = 1200

// AddLink links two posts in one group. Source says who asked: "author"
// (the "Link to this" button), "mod", or "auto" (the check job, via
// SetCheck). NoteA and NoteB are ids for the two notes, used if new.
type AddLink struct {
	GroupID int64
	PostA   int64
	PostB   int64
	Source  string
	By      int64
	NoteA   int64
	NoteB   int64
	At      int64
}

func (c *AddLink) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		return addLink(tx, c.GroupID, c.PostA, c.PostB, c.Source, c.By, c.NoteA, c.NoteB, c.At)
	})
}

func addLink(tx *sql.Tx, groupID, a, b int64, source string, by, noteA, noteB, at int64) error {
	if a == b {
		return Invalid("a post can't be linked to itself")
	}
	older, newer := min(a, b), max(a, b)
	for _, id := range []int64{older, newer} {
		var status string
		if err := tx.QueryRow(`SELECT status FROM posts WHERE id = ?`, id).Scan(&status); err != nil {
			return notFoundGone(err)
		}
		if status != "visible" && status != "flagged" {
			return ErrGone
		}
	}
	var state string
	err := tx.QueryRow(`SELECT state FROM post_links WHERE older_post_id = ? AND newer_post_id = ?`, older, newer).Scan(&state)
	switch {
	case err == nil && state == "active":
		return nil // already linked
	case err == nil && source == "auto":
		return nil // a person rejected this pair; the AI doesn't get to relink it
	case err == nil:
		// A person is relinking a pair that was removed.
		if _, err := tx.Exec(`UPDATE post_links SET state = 'active', source = ?, created_by = ? WHERE older_post_id = ? AND newer_post_id = ?`,
			source, nullIfZero(by), older, newer); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.Exec(`INSERT INTO post_links (older_post_id, newer_post_id, source, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
			older, newer, source, nullIfZero(by), at); err != nil {
			return err
		}
	default:
		return err
	}
	if err := linkNote(tx, groupID, a, b, noteA, at); err != nil {
		return err
	}
	return linkNote(tx, groupID, b, a, noteB, at)
}

// linkNote makes (or reactivates) the note on host that points at other,
// placed after the host thread's latest comment: "at the point in the
// thread where the connection was made". It's queued to be written now.
func linkNote(tx *sql.Tx, groupID, host, other, id, at int64) error {
	var existing int64
	err := tx.QueryRow(`SELECT n.id FROM notes n JOIN note_sources s ON s.note_id = n.id
		WHERE n.host_post_id = ? AND n.kind = 'link' AND s.group_id = ? AND s.post_id = ?`, host, groupID, other).Scan(&existing)
	switch {
	case err == nil:
		id = existing
		if _, err := tx.Exec(`UPDATE notes SET state = 'active', removed_by = NULL, stale = 1 WHERE id = ?`, id); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		var after int64
		tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM comments WHERE post_id = ? AND status IN ('visible', 'flagged')`, host).Scan(&after)
		if _, err := tx.Exec(`INSERT INTO notes (id, host_post_id, after_comment_id, kind, created_at, updated_at)
			VALUES (?, ?, ?, 'link', ?, ?)`, id, host, after, at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO note_sources (note_id, group_id, post_id) VALUES (?, ?, ?)`, id, groupID, other); err != nil {
			return err
		}
	default:
		return err
	}
	v, err := noteVersion(tx, id)
	if err != nil {
		return err
	}
	return schedule(tx, JobNote, id, v, at, at)
}

// noteVersion is the version of what a note is written from: the sum of
// its source threads' versions, which goes up whenever any of them changes.
// (Sources in other groups, from M4, count as 0 here; their changes reach
// the note through the cross-group refresh.)
func noteVersion(tx *sql.Tx, noteID int64) (int64, error) {
	var v int64
	err := tx.QueryRow(`SELECT COALESCE(SUM(p.thread_version), 0) FROM note_sources s JOIN posts p ON p.id = s.post_id
		WHERE s.note_id = ?`, noteID).Scan(&v)
	return v, err
}

// RemoveLink unlinks two posts (a mod, or either post's author) and
// remembers the pair as rejected, so it's never linked automatically again.
type RemoveLink struct {
	GroupID int64
	PostA   int64
	PostB   int64
	By      int64
	ByMod   bool
	At      int64
}

func (c *RemoveLink) Apply(a *Applier) (any, error) {
	older, newer := min(c.PostA, c.PostB), max(c.PostA, c.PostB)
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE post_links SET state = 'rejected' WHERE older_post_id = ? AND newer_post_id = ?`, older, newer)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrGone
		}
		if _, err := tx.Exec(`UPDATE notes SET state = 'removed', removed_by = ? WHERE kind = 'link' AND id IN (
			SELECT n.id FROM notes n JOIN note_sources s ON s.note_id = n.id
			WHERE (n.host_post_id = ?1 AND s.post_id = ?2) OR (n.host_post_id = ?2 AND s.post_id = ?1))`,
			c.By, older, newer); err != nil {
			return err
		}
		if c.ByMod {
			return modLog(tx, c.By, "unlink", "post", newer, "", c.At)
		}
		return nil
	})
}

// CheckLink is one match a check job found: link PostID to Other, using
// these ids for the two notes if they're new.
type CheckLink struct {
	Other     int64
	NoteHere  int64
	NoteThere int64
}

// SetCheck is a check job's result for a post. In M2 that's the older
// posts on the same topic; M5 adds the moderation verdict to the same call.
type SetCheck struct {
	GroupID int64
	JobID   int64
	Worker  string
	PostID  int64
	Version int64
	Links   []CheckLink
	At      int64
}

func (c *SetCheck) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRow(`SELECT version FROM posts WHERE id = ?`, c.PostID).Scan(&current); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
		if err != nil || !ok {
			return err
		}
		for _, l := range c.Links {
			err := addLink(tx, c.GroupID, c.PostID, l.Other, "auto", 0, l.NoteHere, l.NoteThere, c.At)
			if err != nil && !IsInput(err) { // a candidate deleted meanwhile is just skipped
				return err
			}
		}
		return nil
	})
}

// SetNote is a note job's result: new text, or "no meaningful change",
// which keeps the text (and its "updated" date) as it was.
type SetNote struct {
	GroupID  int64
	JobID    int64
	Worker   string
	NoteID   int64
	Version  int64
	Text     string
	NoChange bool
	At       int64
}

func (c *SetNote) Apply(a *Applier) (any, error) {
	text := strings.TrimSpace(c.Text)
	if len(text) > MaxNoteLen {
		text = text[:MaxNoteLen]
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		current, err := noteVersion(tx, c.NoteID)
		if err != nil {
			return err
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
		if err != nil || !ok {
			return err
		}
		var old string
		if err := tx.QueryRow(`SELECT text FROM notes WHERE id = ?`, c.NoteID).Scan(&old); err != nil {
			return notFoundGone(err)
		}
		if c.NoChange && old != "" || text == "" {
			_, err := tx.Exec(`UPDATE notes SET stale = 0 WHERE id = ?`, c.NoteID)
			return err
		}
		_, err = tx.Exec(`UPDATE notes SET text = ?, stale = 0, updated_at = ? WHERE id = ?`, text, c.At, c.NoteID)
		return err
	})
}

// SetDigest stores a thread's digest: a few factual lines that search
// reads instead of the whole thread.
type SetDigest struct {
	GroupID int64
	JobID   int64
	Worker  string
	PostID  int64
	Version int64
	Digest  string
	At      int64
}

func (c *SetDigest) Apply(a *Applier) (any, error) {
	digest := strings.TrimSpace(c.Digest)
	if len(digest) > MaxNoteLen {
		digest = digest[:MaxNoteLen]
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRow(`SELECT thread_version FROM posts WHERE id = ?`, c.PostID).Scan(&current); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
		if err != nil || !ok {
			return err
		}
		_, err = tx.Exec(`UPDATE posts SET digest = ?, digest_updated_at = ? WHERE id = ?`, nullIfEmpty(digest), c.At, c.PostID)
		return err
	})
}

// MoveUnder makes a post an Update section of an earlier post: "part 2 of
// my solar install". It keeps its photos and comments; its own address
// redirects to its place in the other thread. Manual only (the author of
// both, or a mod), and MoveOut undoes it. The AI never moves posts.
type MoveUnder struct {
	GroupID int64
	PostID  int64
	UnderID int64
	By      int64
	ByMod   bool
	At      int64
}

func (c *MoveUnder) Apply(a *Applier) (any, error) {
	if c.UnderID >= c.PostID {
		return nil, Invalid("an update can only go under an earlier post")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		for _, id := range []int64{c.PostID, c.UnderID} {
			var status string
			var cont sql.NullInt64
			if err := tx.QueryRow(`SELECT status, continues_post_id FROM posts WHERE id = ?`, id).Scan(&status, &cont); err != nil {
				return notFoundGone(err)
			}
			if status != "visible" && status != "flagged" {
				return ErrGone
			}
			if cont.Valid {
				return Invalid("that post is already an update under another post")
			}
		}
		var n int
		tx.QueryRow(`SELECT COUNT(*) FROM posts WHERE continues_post_id = ?`, c.PostID).Scan(&n)
		if n > 0 {
			return Invalid("this post has updates of its own; move those first")
		}
		if _, err := tx.Exec(`UPDATE posts SET continues_post_id = ?, continued_at = ? WHERE id = ?`, c.UnderID, c.At, c.PostID); err != nil {
			return err
		}
		// The combined thread is newer now.
		if _, err := tx.Exec(`UPDATE posts SET last_activity_at = MAX(last_activity_at,
			(SELECT last_activity_at FROM posts WHERE id = ?1)) WHERE id = ?2`, c.PostID, c.UnderID); err != nil {
			return err
		}
		if c.ByMod {
			if err := modLog(tx, c.By, "move_under", "post", c.PostID, "", c.At); err != nil {
				return err
			}
		}
		return threadChanged(tx, c.GroupID, c.PostID, c.At)
	})
}

// MoveOut undoes MoveUnder: the post stands on its own again.
type MoveOut struct {
	GroupID int64
	PostID  int64
	By      int64
	ByMod   bool
	At      int64
}

func (c *MoveOut) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var under sql.NullInt64
		if err := tx.QueryRow(`SELECT continues_post_id FROM posts WHERE id = ?`, c.PostID).Scan(&under); err != nil {
			return notFoundGone(err)
		}
		if !under.Valid {
			return nil
		}
		// threadChanged first, while the post still points at the thread it
		// is leaving, so both threads' digests are refreshed.
		if err := threadChanged(tx, c.GroupID, c.PostID, c.At); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE posts SET continues_post_id = NULL, continued_at = NULL WHERE id = ?`, c.PostID); err != nil {
			return err
		}
		if c.ByMod {
			return modLog(tx, c.By, "move_out", "post", c.PostID, "", c.At)
		}
		return nil
	})
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
