package cmd

import (
	"database/sql"
	"sort"
)

// Follow-up commands: what one log's command sends to another log through
// its outbox (see logs.go). Each is safe to apply twice, because the relay
// may deliver one again after a crash.

// InitGroup gives a new group's own file its first owner, and its copy of
// the group's first settings (site.db has the real ones; see
// UpdateSettings). CreateGroup, on the site log, sends it to the new
// group's log.
type InitGroup struct {
	GroupID     int64
	Name        string
	Description string
	Visibility  string
	OwnerID     int64
	At          int64
}

func (c *InitGroup) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		// A private group's FAQ starts private too.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO settings (id, name, description, visibility, public_faq) VALUES (1, ?, ?, ?, ?)`,
			c.Name, c.Description, c.Visibility, c.Visibility == "public"); err != nil {
			return err
		}
		if c.OwnerID != 0 {
			_, err := tx.Exec(`INSERT OR IGNORE INTO memberships (user_id, role, status, created_at) VALUES (?, 'owner', 'active', ?)`,
				c.OwnerID, c.At)
			return err
		}
		return nil
	})
}

// MirrorGroup updated site.db's copy of a group's name, visibility and
// "Use AI" setting, when settings lived in the group's own file. Settings
// live in site.db now (UpdateSettings); this stays so that old logs, and
// any still waiting in an outbox, apply as they did.
type MirrorGroup struct {
	GroupID    int64
	Name       string
	Visibility string
	AIEnabled  bool
}

func (*MirrorGroup) siteLog() {}

func (c *MirrorGroup) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE groups SET name = ?, visibility = ?, ai_enabled = ? WHERE id = ?`,
			c.Name, c.Visibility, c.AIEnabled, c.GroupID)
		return err
	})
}

// DropSisterLinks takes down one group's links and notes to a former
// sister (EndSister sends one to each side). A link a mod rejected stays
// rejected: pairing again doesn't undo that.
type DropSisterLinks struct {
	GroupID int64
	Other   int64
	By      int64 // for the mod log, on the side that ended it; 0 on the other
	At      int64
}

func (c *DropSisterLinks) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sister_links WHERE other_group = ? AND state = 'active'`, c.Other); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE notes SET state = 'removed' WHERE kind = 'link' AND state = 'active'
			AND id IN (SELECT note_id FROM note_sources WHERE group_id = ?)`, c.Other); err != nil {
			return err
		}
		if c.By != 0 {
			return modLog(tx, c.By, "sister_end", "group", c.Other, "", c.At)
		}
		return nil
	})
}

// PutSisterSide is a sister group's side of a new cross-link, sent by the
// check that found it: the link row on the sister's post, and a note there
// citing FromPost when the visibility rule allows (Cite).
type PutSisterSide struct {
	GroupID   int64
	Post      int64 // the sister group's post
	FromGroup int64
	FromPost  int64
	Title     string // FromPost's title, date and thread version
	Date      int64
	Version   int64
	NoteID    int64
	Cite      bool
	At        int64
}

func (c *PutSisterSide) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var status string
		if tx.QueryRow(`SELECT status FROM posts WHERE id = ?`, c.Post).Scan(&status) != nil || !shown(status) {
			return nil // gone meanwhile: nothing to link
		}
		ok, err := putSisterLink(tx, c.Post, c.FromGroup, c.FromPost, c.Title, c.Date, "auto", c.At)
		if err != nil || !ok || !c.Cite {
			return err
		}
		return linkNote(tx, c.FromGroup, c.Post, c.FromPost, c.NoteID, c.Version, c.At)
	})
}

// ShowSisterNotes hides (or shows again) the notes in one sister group
// that are written from FromPost, when that post is deleted or hidden (or
// comes back). A note's text is made from the post, so it must go when
// the post does. Only notes taken down this way come back, not ones a mod
// removed.
type ShowSisterNotes struct {
	GroupID   int64
	Hosts     []int64 // the posts here the notes sit on
	FromGroup int64
	FromPost  int64
	Show      bool
}

func (c *ShowSisterNotes) Apply(a *Applier) (any, error) {
	from, to := "active", "removed"
	if c.Show {
		from, to = to, from
	}
	if len(c.Hosts) == 0 {
		return nil, nil
	}
	hosts := make([]any, len(c.Hosts))
	for i, h := range c.Hosts {
		hosts[i] = h
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notes SET state = ? WHERE kind = 'link' AND state = ? AND removed_by IS NULL
			AND host_post_id IN (`+placeholders(len(hosts))+`)
			AND id IN (SELECT note_id FROM note_sources WHERE group_id = ? AND post_id = ?)`,
			append(append([]any{to, from}, hosts...), c.FromGroup, c.FromPost)...)
		return err
	})
}

// sendSisterNotes sends ShowSisterNotes to each sister group a post is
// linked to, from inside the post's own transaction.
func sendSisterNotes(tx *sql.Tx, groupID, post int64, refs []sisterRef, show bool, at int64) error {
	byGroup := map[int64][]int64{}
	var groups []int64
	for _, r := range refs {
		if byGroup[r.group] == nil {
			groups = append(groups, r.group)
		}
		byGroup[r.group] = append(byGroup[r.group], r.post)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	for _, g := range groups {
		if err := send(tx, &ShowSisterNotes{GroupID: g, Hosts: byGroup[g], FromGroup: groupID, FromPost: post, Show: show}, at); err != nil {
			return err
		}
	}
	return nil
}

// groupIDs lists every group in site.db, for site commands that send
// something to each group's log.
func groupIDs(tx *sql.Tx, where string) ([]int64, error) {
	return queryIDs(tx, `SELECT id FROM groups `+where+` ORDER BY id`)
}
