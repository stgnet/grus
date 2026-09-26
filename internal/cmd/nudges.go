package cmd

import (
	"database/sql"
	"errors"
	"strings"
)

// Keeping long threads readable (plan section 2, "Keeping it organized:
// summarize over, never rewrite"). Nothing here changes what a member
// wrote. A summary note sits at the top of a long thread with the comments
// it covers folded under it, one tap from exactly as written; nudges move
// the comments that carry the answer up, fold off-topic stretches to one
// line, and mark advice a later comment replaced. All of it is display
// only, reversible by a mod, and gone with "show in order".

// Tangent is an off-topic stretch of a thread: comments From through To,
// about something else ("tire pressure").
type Tangent struct {
	From  int64
	To    int64
	About string
}

// Supersede marks a comment whose advice a later comment in the thread
// replaced.
type Supersede struct {
	Comment int64
	By      int64
}

// SetSummary is a summary job's result for a long thread: the summary
// note's text, the comments it covers, and the thread's nudges. NoteID is
// the id to use if the thread has no summary note yet.
type SetSummary struct {
	GroupID    int64
	JobID      int64
	Worker     string
	PostID     int64
	Version    int64
	NoteID     int64
	Text       string
	Covers     []int64
	Useful     []int64
	Tangents   []Tangent
	Superseded []Supersede
	At         int64
}

func (c *SetSummary) Apply(a *Applier) (any, error) {
	text := strings.TrimSpace(c.Text)
	if len(text) > MaxNoteLen {
		text = text[:MaxNoteLen]
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
		// Only this thread's own shown comments can be covered or nudged:
		// a model that invents or misnumbers a comment is ignored.
		shown := map[int64]bool{}
		ids, err := queryIDs(tx, `SELECT id FROM comments WHERE post_id = ? AND status IN ('visible', 'flagged')`, c.PostID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			shown[id] = true
		}
		if err := setSummaryNote(tx, c, text, shown); err != nil {
			return err
		}
		return setNudges(tx, c, shown)
	})
}

func setSummaryNote(tx *sql.Tx, c *SetSummary, text string, shown map[int64]bool) error {
	var id int64
	var removedBy sql.NullInt64
	err := tx.QueryRow(`SELECT id, removed_by FROM notes WHERE host_post_id = ? AND kind = 'summary'`, c.PostID).Scan(&id, &removedBy)
	switch {
	case err == nil && removedBy.Valid:
		return nil // a mod removed the summary, which unfolds everything; it stays that way
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		if text == "" || len(c.Covers) == 0 {
			return nil
		}
		id = c.NoteID
		if _, err := tx.Exec(`INSERT INTO notes (id, host_post_id, after_comment_id, kind, created_at, updated_at)
			VALUES (?, ?, 0, 'summary', ?, ?)`, id, c.PostID, c.At, c.At); err != nil {
			return err
		}
	default:
		return err
	}
	if text == "" {
		_, err := tx.Exec(`UPDATE notes SET state = 'removed' WHERE id = ?`, id)
		return err
	}
	if _, err := tx.Exec(`UPDATE notes SET text = ?, stale = 0, state = 'active', updated_at = ? WHERE id = ?`, text, c.At, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM note_sources WHERE note_id = ?`, id); err != nil {
		return err
	}
	for _, cm := range c.Covers {
		if !shown[cm] {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO note_sources (note_id, group_id, post_id, comment_id, covers) VALUES (?, ?, ?, ?, 1)`,
			id, c.GroupID, c.PostID, cm); err != nil {
			return err
		}
	}
	return nil
}

// setNudges replaces the system's nudges on a thread with this run's,
// leaving alone any a mod reversed (and not making those again).
func setNudges(tx *sql.Tx, c *SetSummary, shown map[int64]bool) error {
	if _, err := tx.Exec(`DELETE FROM nudges WHERE post_id = ? AND kind IN ('rank', 'tangent', 'superseded')
		AND state = 'active' AND set_by IS NULL`, c.PostID); err != nil {
		return err
	}
	add := func(kind string, target, value int64, reason string) error {
		if !shown[target] {
			return nil
		}
		var reversed int
		tx.QueryRow(`SELECT COUNT(*) FROM nudges WHERE post_id = ? AND kind = ? AND target_id = ? AND state = 'reversed'`,
			c.PostID, kind, target).Scan(&reversed)
		if reversed > 0 {
			return nil
		}
		_, err := tx.Exec(`INSERT INTO nudges (id, post_id, kind, target_id, value, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rowID("nudge", c.PostID, kind, target, c.At), c.PostID, kind, target, value, reason, c.At)
		return err
	}
	for i, cm := range c.Useful {
		if i == 5 {
			break // a few answers up top; more is just reordering the thread
		}
		if err := add("rank", cm, int64(i), "carries the answer"); err != nil {
			return err
		}
	}
	for _, t := range c.Tangents {
		about := strings.Join(strings.Fields(t.About), " ")
		if len(about) > 80 {
			about = about[:80]
		}
		if about == "" || !shown[t.To] || t.To < t.From {
			continue
		}
		if err := add("tangent", t.From, t.To, about); err != nil {
			return err
		}
	}
	for _, s := range c.Superseded {
		if !shown[s.By] || s.By <= s.Comment {
			continue
		}
		if err := add("superseded", s.Comment, s.By, "newer information below"); err != nil {
			return err
		}
	}
	return nil
}

// ReverseNudge undoes a nudge (mods). It stays in the table as reversed, so
// the system doesn't make the same nudge again.
type ReverseNudge struct {
	GroupID int64
	NudgeID int64
	By      int64
	At      int64
}

func (c *ReverseNudge) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var kind string
		var target int64
		if err := tx.QueryRow(`SELECT kind, target_id FROM nudges WHERE id = ?`, c.NudgeID).Scan(&kind, &target); err != nil {
			return notFoundGone(err)
		}
		if _, err := tx.Exec(`UPDATE nudges SET state = 'reversed', set_by = ? WHERE id = ?`, c.By, c.NudgeID); err != nil {
			return err
		}
		if kind == "feed_weight" {
			if _, err := tx.Exec(`UPDATE posts SET sink = 0 WHERE id = ?`, target); err != nil {
				return err
			}
		}
		return modLog(tx, c.By, "nudge_reverse", kind, target, "", c.At)
	})
}

// RemoveNote takes down a summary or combined note (mods). Removing a
// summary unfolds the comments under it. It stays down: the system won't
// write it again.
type RemoveNote struct {
	GroupID int64
	NoteID  int64
	By      int64
	At      int64
}

func (c *RemoveNote) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var kind string
		var host int64
		if err := tx.QueryRow(`SELECT kind, host_post_id FROM notes WHERE id = ?`, c.NoteID).Scan(&kind, &host); err != nil {
			return notFoundGone(err)
		}
		if kind == "link" {
			return Invalid("unlink the posts instead")
		}
		if _, err := tx.Exec(`UPDATE notes SET state = 'removed', removed_by = ? WHERE id = ?`, c.By, c.NoteID); err != nil {
			return err
		}
		return modLog(tx, c.By, "note_remove", "post", host, kind, c.At)
	})
}
