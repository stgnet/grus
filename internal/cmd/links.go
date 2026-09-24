package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
// SetCheck). NoteA and NoteB are ids for the two notes, used if new;
// CombinedA and CombinedB are ids for a combined note on either post, used
// if this link is the one that makes a post need one.
type AddLink struct {
	GroupID   int64
	PostA     int64
	PostB     int64
	Source    string
	By        int64
	NoteA     int64
	NoteB     int64
	CombinedA int64
	CombinedB int64
	At        int64
}

func (c *AddLink) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		return addLink(tx, c.GroupID, c.PostA, c.PostB, c.Source, c.By, [4]int64{c.NoteA, c.NoteB, c.CombinedA, c.CombinedB}, c.At)
	})
}

// FeedSink is how much older a repeat of a well-answered thread is treated
// in the Active feed, so the same question every month doesn't dominate it.
const FeedSink = 12 * 3600

// CombinedMin is how many related threads (and outside pages) a post needs
// before it gets a combined note gathering them into one answer.
const CombinedMin = 3

// addLink links a and b. ids holds the note ids to use if new: the link
// note on a, on b, and a combined note on a, on b.
func addLink(tx *sql.Tx, groupID, a, b int64, source string, by int64, ids [4]int64, at int64) error {
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
	if err := linkNote(tx, groupID, a, b, ids[0], 0, at); err != nil {
		return err
	}
	if err := linkNote(tx, groupID, b, a, ids[1], 0, at); err != nil {
		return err
	}
	if err := feedWeight(tx, older, newer, at); err != nil {
		return err
	}
	// Linked threads are about the same thing, so an entry written from
	// one of them takes in the other: a repeat question joins the entry
	// that covers it, and what it adds flows back at the next rewrite.
	entries, err := queryIDs(tx, `SELECT DISTINCT s.entry_id FROM faq_sources s JOIN faq_entries e ON e.id = s.entry_id
		WHERE s.post_id IN (?, ?) AND e.status = 'active'`, older, newer)
	if err != nil {
		return err
	}
	for _, e := range entries {
		for _, p := range []int64{older, newer} {
			if err := addFAQSource(tx, e, p); err != nil {
				return err
			}
		}
	}
	if err := syncCombined(tx, groupID, a, ids[2], at); err != nil {
		return err
	}
	return syncCombined(tx, groupID, b, ids[3], at)
}

// feedWeight lowers a new post that repeats a well-answered thread (one
// with real discussion, or in the FAQ) in the Active feed. It's a nudge:
// recorded with its reason, and a mod can reverse it for good.
func feedWeight(tx *sql.Tx, older, newer, at int64) error {
	var comments int
	var inFAQ bool
	if err := tx.QueryRow(`SELECT comment_count, EXISTS (SELECT 1 FROM faq_sources s JOIN faq_entries e ON e.id = s.entry_id
		WHERE s.post_id = posts.id AND e.status = 'active') FROM posts WHERE id = ?`, older).Scan(&comments, &inFAQ); err != nil {
		return err
	}
	if comments < FAQMinComments && !inFAQ {
		return nil
	}
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM nudges WHERE kind = 'feed_weight' AND target_id = ?`, newer).Scan(&n)
	if n > 0 {
		return nil // already weighted, or a mod reversed it
	}
	if _, err := tx.Exec(`INSERT INTO nudges (post_id, kind, target_id, value, reason, created_at)
		VALUES (?, 'feed_weight', ?, ?, ?, ?)`, newer, newer, FeedSink, fmt.Sprintf("repeats answered thread %d", older), at); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE posts SET sink = ? WHERE id = ?`, FeedSink, newer)
	return err
}

// syncCombined keeps a post's combined note in step with what it's linked
// to: made (with id newID) once a post has CombinedMin related threads and
// outside pages, its sources kept to exactly those, refreshed when they
// change, and taken down below the threshold. A combined note a mod
// removed stays removed. newID 0 = don't create one now.
func syncCombined(tx *sql.Tx, groupID, host, newID, at int64) error {
	type src struct{ post, source int64 }
	var want []src
	posts, err := queryIDs(tx, `SELECT CASE WHEN l.older_post_id = ?1 THEN l.newer_post_id ELSE l.older_post_id END AS other
		FROM post_links l JOIN posts p ON p.id = (CASE WHEN l.older_post_id = ?1 THEN l.newer_post_id ELSE l.older_post_id END)
		WHERE (l.older_post_id = ?1 OR l.newer_post_id = ?1) AND l.state = 'active' AND p.status IN ('visible', 'flagged')
		ORDER BY other`, host)
	if err != nil {
		return err
	}
	for _, p := range posts {
		want = append(want, src{p, 0})
	}
	outside, err := queryIDs(tx, `SELECT s.id FROM source_links l JOIN sources s ON s.id = l.source_id
		WHERE l.post_id = ? AND s.status = 'active' AND s.summary != '' ORDER BY s.id`, host)
	if err != nil {
		return err
	}
	for _, s := range outside {
		want = append(want, src{0, s})
	}
	var id int64
	var state string
	var removedBy sql.NullInt64
	err = tx.QueryRow(`SELECT id, state, removed_by FROM notes WHERE host_post_id = ? AND kind = 'combined'`, host).Scan(&id, &state, &removedBy)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(want) < CombinedMin {
		if exists && state == "active" {
			_, err := tx.Exec(`UPDATE notes SET state = 'removed' WHERE id = ?`, id)
			return err
		}
		return nil
	}
	if exists && removedBy.Valid {
		return nil // a mod took it down
	}
	if !exists {
		if newID == 0 {
			return nil
		}
		id = newID
		if _, err := tx.Exec(`INSERT INTO notes (id, host_post_id, after_comment_id, kind, created_at, updated_at)
			VALUES (?, ?, 0, 'combined', ?, ?)`, id, host, at, at); err != nil {
			return err
		}
	}
	// Same sources as before and already showing: nothing to redo.
	rows, err := tx.Query(`SELECT post_id, source_id FROM note_sources WHERE note_id = ? ORDER BY source_id != 0, post_id, source_id`, id)
	if err != nil {
		return err
	}
	var have []src
	for rows.Next() {
		var s src
		rows.Scan(&s.post, &s.source)
		have = append(have, s)
	}
	rows.Close()
	if exists && state == "active" && slices.Equal(have, want) {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM note_sources WHERE note_id = ?`, id); err != nil {
		return err
	}
	for _, s := range want {
		if _, err := tx.Exec(`INSERT INTO note_sources (note_id, group_id, post_id, source_id) VALUES (?, ?, ?, ?)`,
			id, groupID, s.post, s.source); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE notes SET state = 'active', stale = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	v, err := noteVersion(tx, id)
	if err != nil {
		return err
	}
	return schedule(tx, JobNote, id, v, at, at)
}

// linkNote makes (or reactivates) the note on host that points at other,
// placed after the host thread's latest comment: "at the point in the
// thread where the connection was made". It's queued to be written now.
//
// groupID is the group other is in: this group, or a sister group (M4),
// in which case ext is the other thread's version as the worker read it
// (see noteVersion).
func linkNote(tx *sql.Tx, groupID, host, other, id, ext, at int64) error {
	var existing int64
	err := tx.QueryRow(`SELECT n.id FROM notes n JOIN note_sources s ON s.note_id = n.id
		WHERE n.host_post_id = ? AND n.kind = 'link' AND s.group_id = ? AND s.post_id = ?`, host, groupID, other).Scan(&existing)
	switch {
	case err == nil:
		id = existing
		if _, err := tx.Exec(`UPDATE notes SET state = 'active', removed_by = NULL, stale = 1,
			ext_version = MAX(ext_version, ?) WHERE id = ?`, ext, id); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		var after int64
		tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM comments WHERE post_id = ? AND status IN ('visible', 'flagged')`, host).Scan(&after)
		if _, err := tx.Exec(`INSERT INTO notes (id, host_post_id, after_comment_id, kind, ext_version, created_at, updated_at)
			VALUES (?, ?, ?, 'link', ?, ?, ?)`, id, host, after, ext, at, at); err != nil {
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
// A source in a sister group isn't in this file (ids are unique across
// groups, so it simply doesn't join); its version is carried in the note's
// ext_version, which the worker's sister sweep raises (MarkSisterStale).
// store.NoteVersion must compute the same thing.
func noteVersion(tx *sql.Tx, noteID int64) (int64, error) {
	var v int64
	err := tx.QueryRow(`SELECT COALESCE((SELECT SUM(p.thread_version) FROM note_sources s JOIN posts p ON p.id = s.post_id
		WHERE s.note_id = ?1), 0) + COALESCE((SELECT ext_version FROM notes WHERE id = ?1), 0)`, noteID).Scan(&v)
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
		if _, err := tx.Exec(`UPDATE notes SET state = 'removed', removed_by = ?3 WHERE kind = 'link' AND id IN (
			SELECT n.id FROM notes n JOIN note_sources s ON s.note_id = n.id
			WHERE (n.host_post_id = ?1 AND s.post_id = ?2) OR (n.host_post_id = ?2 AND s.post_id = ?1))`,
			older, newer, c.By); err != nil {
			return err
		}
		for _, p := range []int64{older, newer} {
			if err := syncCombined(tx, c.GroupID, p, 0, c.At); err != nil {
				return err
			}
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
	Other         int64
	NoteHere      int64
	NoteThere     int64
	CombinedHere  int64
	CombinedThere int64
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
	Entries []int64       // FAQ entries that already cover this post's question
	Sources []int64       // outside pages about the same thing
	Sisters []SisterMatch // posts in sister groups about the same thing (M4)
	At      int64
}

func (c *SetCheck) Apply(a *Applier) (any, error) {
	plans, err := sisterPlans(a, c.GroupID, c.Sisters)
	if err != nil {
		return nil, err
	}
	var kept []sisterPlan
	var title string
	var created, threadVersion int64
	err = a.Group(c.GroupID, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRow(`SELECT version, thread_version, title, created_at FROM posts WHERE id = ?`, c.PostID).
			Scan(&current, &threadVersion, &title, &created); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
		if err != nil || !ok {
			return err
		}
		if kept, err = sisterHere(tx, c.PostID, plans, c.At); err != nil {
			return err
		}
		for _, l := range c.Links {
			err := addLink(tx, c.GroupID, c.PostID, l.Other, "auto", 0,
				[4]int64{l.NoteHere, l.NoteThere, l.CombinedHere, l.CombinedThere}, c.At)
			if err != nil && !IsInput(err) { // a candidate deleted meanwhile is just skipped
				return err
			}
		}
		// "Covered in the FAQ": the post joins the entry, which shows on
		// the post and takes in whatever the thread adds.
		for _, e := range c.Entries {
			var status string
			if tx.QueryRow(`SELECT status FROM faq_entries WHERE id = ?`, e).Scan(&status) != nil || status != "active" {
				continue
			}
			if err := addFAQSource(tx, e, c.PostID); err != nil {
				return err
			}
		}
		for _, s := range c.Sources {
			var status string
			if tx.QueryRow(`SELECT status FROM sources WHERE id = ?`, s).Scan(&status) != nil || status != "active" {
				continue
			}
			if err := linkSource(tx, c.GroupID, s, c.PostID, 0, c.At); err != nil {
				return err
			}
		}
		return nil
	})
	// On a replay after a crash, this group's part may already be done, and
	// then kept is empty and the sister groups' parts are skipped too, even
	// if the crash came between them. That loses at most a few links on the
	// other side, which the next check of that group's posts finds again.
	if err != nil || len(kept) == 0 {
		return nil, err
	}
	return nil, sisterThere(a, c.GroupID, c.PostID, title, created, threadVersion, kept, c.At)
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
//
// Topics are the post's topic tags from the same job (plan section 2, "Topic
// tags"), set only when TopicsDone (the group has topics to choose from),
// and never on a post whose author or a mod chose its topics.
type SetDigest struct {
	GroupID    int64
	JobID      int64
	Worker     string
	PostID     int64
	Version    int64
	Digest     string
	Topics     []int64
	TopicsDone bool
	At         int64
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
		if _, err := tx.Exec(`UPDATE posts SET digest = ?, digest_updated_at = ? WHERE id = ?`, nullIfEmpty(digest), c.At, c.PostID); err != nil {
			return err
		}
		if c.TopicsDone {
			return setTopics(tx, c.PostID, c.Topics, "auto", false)
		}
		return nil
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
