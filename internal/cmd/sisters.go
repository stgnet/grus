package cmd

import (
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/stgnet/grus/internal/auth"
)

// Sister groups (plan section 2): two groups that share territory, like a
// Travato group and a ProMaster group, get link notes between their posts.
//
// The pairing lives in site.db (group_pairs). The links and notes live in
// the groups' own files, each note in the group that shows it, so a node
// holding one group can show a note citing the other without holding it.
//
// Commands that write two groups' files do it as two transactions, one per
// file, and never read the other group's file: in M7 a node may hold only
// one of them. What they need to know about the other group (visibility,
// "Use AI", whether the pair is active) comes from site.db, which every
// node holds.

// MaxSisterTopics caps the topic words that limit a pairing.
const MaxSisterTopics = 200

// Pair is how two groups are paired, from site.db.
type Pair struct {
	Active bool
	Topics string
}

// pairKey orders a pair's ids the way group_pairs stores them.
func pairKey(g1, g2 int64) (int64, int64) { return min(g1, g2), max(g1, g2) }

// sisterPair reads a pairing from site.db.
func sisterPair(site *sql.DB, g1, g2 int64) (Pair, error) {
	a, b := pairKey(g1, g2)
	var p Pair
	var state string
	err := site.QueryRow(`SELECT state, topics FROM group_pairs WHERE group_a = ? AND group_b = ?`, a, b).Scan(&state, &p.Topics)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	p.Active = state == "active"
	return p, err
}

// groupFacts are the copies of a group's settings site.db keeps.
func groupFacts(q interface {
	QueryRow(string, ...any) *sql.Row
}, id int64) (visibility string, ai bool, err error) {
	err = q.QueryRow(`SELECT visibility, ai_enabled FROM groups WHERE id = ?`, id).Scan(&visibility, &ai)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return visibility, ai, err
}

// ProposeSister is an owner of GroupID asking Other to pair. If Other
// already asked, this accepts instead.
type ProposeSister struct {
	GroupID int64
	Other   int64
	Topics  string
	By      int64
	At      int64
}

func (c *ProposeSister) Apply(a *Applier) (any, error) {
	// "chassis,engine , suspension" is kept as "chassis, engine, suspension".
	var words []string
	for _, w := range strings.Split(c.Topics, ",") {
		if w = strings.Join(strings.Fields(w), " "); w != "" {
			words = append(words, w)
		}
	}
	topics := strings.Join(words, ", ")
	if len(topics) > MaxSisterTopics {
		return nil, Invalid("topics can be up to %d characters", MaxSisterTopics)
	}
	if c.Other == c.GroupID {
		return nil, Invalid("a group can't be its own sister")
	}
	ga, gb := pairKey(c.GroupID, c.Other)
	var state string
	err := a.Site(func(tx *sql.Tx) error {
		for _, g := range []int64{c.GroupID, c.Other} {
			_, ai, err := groupFacts(tx, g)
			if err != nil {
				return err
			}
			// Sister notes are written by the AI, so both groups must have
			// said yes to it.
			if !ai {
				return Invalid("both groups need \"Use AI in this group\" on")
			}
		}
		var old string
		var by int64
		err := tx.QueryRow(`SELECT state, proposed_by_group FROM group_pairs WHERE group_a = ? AND group_b = ?`, ga, gb).Scan(&old, &by)
		switch {
		case err == nil && old == "active":
			return Invalid("these groups are already sisters")
		case err == nil && old == "proposed" && by == c.Other:
			state = "active" // they asked us: this is a yes
			_, err = tx.Exec(`UPDATE group_pairs SET state = 'active', decided_by = ?, updated_at = ? WHERE group_a = ? AND group_b = ?`,
				c.By, c.At, ga, gb)
			return err
		case err == nil || errors.Is(err, sql.ErrNoRows):
			state = "proposed"
			_, err = tx.Exec(`INSERT INTO group_pairs (group_a, group_b, proposed_by_group, topics, state, proposed_by, created_at, updated_at)
				VALUES (?, ?, ?, ?, 'proposed', ?, ?, ?)
				ON CONFLICT (group_a, group_b) DO UPDATE SET proposed_by_group = excluded.proposed_by_group, topics = excluded.topics,
				  state = 'proposed', proposed_by = excluded.proposed_by, decided_by = NULL, updated_at = excluded.updated_at`,
				ga, gb, c.GroupID, topics, c.By, c.At, c.At)
			return err
		default:
			return err
		}
	})
	if err != nil {
		return nil, err
	}
	return state, a.Group(c.GroupID, func(tx *sql.Tx) error {
		return modLog(tx, c.By, "sister_"+state, "group", c.Other, topics, c.At)
	})
}

// AnswerSister is an owner of GroupID answering Other's proposal.
type AnswerSister struct {
	GroupID int64
	Other   int64
	Accept  bool
	By      int64
	At      int64
}

func (c *AnswerSister) Apply(a *Applier) (any, error) {
	ga, gb := pairKey(c.GroupID, c.Other)
	state := "ended"
	if c.Accept {
		state = "active"
	}
	err := a.Site(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE group_pairs SET state = ?, decided_by = ?, updated_at = ?
			WHERE group_a = ? AND group_b = ? AND state = 'proposed' AND proposed_by_group = ?`,
			state, c.By, c.At, ga, gb, c.Other)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Invalid("that proposal has already been answered")
		}
		if c.Accept {
			// Checked again now: "Use AI" may have been turned off since.
			for _, g := range []int64{c.GroupID, c.Other} {
				if _, ai, err := groupFacts(tx, g); err != nil || !ai {
					return Invalid("both groups need \"Use AI in this group\" on")
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		return modLog(tx, c.By, "sister_"+map[bool]string{true: "accept", false: "decline"}[c.Accept], "group", c.Other, "", c.At)
	})
}

// EndSister ends a pairing (or withdraws a proposal), from either side,
// and takes down every link between the two groups.
type EndSister struct {
	GroupID int64
	Other   int64
	By      int64
	At      int64
}

func (c *EndSister) Apply(a *Applier) (any, error) {
	ga, gb := pairKey(c.GroupID, c.Other)
	err := a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE group_pairs SET state = 'ended', decided_by = ?, updated_at = ? WHERE group_a = ? AND group_b = ?`,
			c.By, c.At, ga, gb)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Each side's links and notes to the other. A rejected link stays
	// rejected (a mod said no to it; pairing again doesn't undo that).
	for _, pair := range [][2]int64{{c.GroupID, c.Other}, {c.Other, c.GroupID}} {
		here, there := pair[0], pair[1]
		err := a.Group(here, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM sister_links WHERE other_group = ? AND state = 'active'`, there); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE notes SET state = 'removed' WHERE kind = 'link' AND state = 'active'
				AND id IN (SELECT note_id FROM note_sources WHERE group_id = ?)`, there); err != nil {
				return err
			}
			if here == c.GroupID {
				return modLog(tx, c.By, "sister_end", "group", there, "", c.At)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// SisterMatch is one post in a sister group that a check job found to be
// about the same thing: the note ids to use on each side, and the other
// thread's version as the worker read it.
type SisterMatch struct {
	Group     int64
	Post      int64
	Version   int64
	Title     string // the other post's title and date, for the note's heading
	Date      int64
	NoteHere  int64
	NoteThere int64
}

// Sister links are SetCheck's cross-group part, in three steps:
//
//  1. sisterPlans, before any transaction: which matches may be linked at
//     all, and which side of each gets a note.
//  2. sisterHere, inside SetCheck's own transaction on this group's file
//     (a second transaction on the same file in one command would be
//     skipped as already applied): this side's link rows and notes.
//  3. sisterThere, after it: each sister group's side, one transaction
//     per group.
//
// The visibility rule (auth.CanCite) decides which side gets a note: a
// note on this post needs the other group to be public, and a note on the
// other post needs this group to be public. Group facts come from site.db;
// see the Applier's note on reading one file while writing another: a
// replay may see a later pairing state, and then the later EndSister (also
// replayed) removes whatever this adds, so the files end up the same.

type sisterPlan struct {
	m               SisterMatch
	noteHere, there bool
}

func sisterPlans(a *Applier, groupID int64, matches []SisterMatch) ([]sisterPlan, error) {
	if len(matches) == 0 {
		return nil, nil
	}
	site := a.Store.Site()
	visHere, aiHere, err := groupFacts(site, groupID)
	if err != nil || !aiHere {
		return nil, nil
	}
	var plans []sisterPlan
	for _, m := range matches {
		p, err := sisterPair(site, groupID, m.Group)
		if err != nil {
			return nil, err
		}
		visThere, aiThere, err := groupFacts(site, m.Group)
		if err != nil || !p.Active || !aiThere {
			continue
		}
		pl := sisterPlan{m: m, noteHere: auth.CanCite(visThere), there: auth.CanCite(visHere)}
		if pl.noteHere || pl.there {
			plans = append(plans, pl)
		}
	}
	return plans, nil
}

// sisterHere writes this group's side. It returns the plans a mod hasn't
// rejected, for sisterThere.
func sisterHere(tx *sql.Tx, postID int64, plans []sisterPlan, at int64) ([]sisterPlan, error) {
	var kept []sisterPlan
	for _, pl := range plans {
		ok, err := putSisterLink(tx, postID, pl.m.Group, pl.m.Post, pl.m.Title, pl.m.Date, "auto", at)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a mod rejected this pair
		}
		kept = append(kept, pl)
		if pl.noteHere {
			if err := linkNote(tx, pl.m.Group, postID, pl.m.Post, pl.m.NoteHere, pl.m.Version, at); err != nil {
				return nil, err
			}
		}
	}
	return kept, nil
}

// sisterThere writes each sister group's side: its link row, and a note
// on its post where the rule allows. title, date and version are this
// post's, as of this command.
func sisterThere(a *Applier, groupID, postID int64, title string, date, version int64, kept []sisterPlan, at int64) error {
	byGroup := map[int64][]sisterPlan{}
	var groups []int64
	for _, pl := range kept {
		if byGroup[pl.m.Group] == nil {
			groups = append(groups, pl.m.Group)
		}
		byGroup[pl.m.Group] = append(byGroup[pl.m.Group], pl)
	}
	slices.Sort(groups)
	for _, g := range groups {
		err := a.Group(g, func(tx *sql.Tx) error {
			for _, pl := range byGroup[g] {
				var status string
				if tx.QueryRow(`SELECT status FROM posts WHERE id = ?`, pl.m.Post).Scan(&status) != nil ||
					(status != "visible" && status != "flagged") {
					continue
				}
				ok, err := putSisterLink(tx, pl.m.Post, groupID, postID, title, date, "auto", at)
				if err != nil {
					return err
				}
				if ok && pl.there {
					if err := linkNote(tx, groupID, pl.m.Post, postID, pl.m.NoteThere, version, at); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// putSisterLink records an active link from post to (group, other) on this
// side, unless it was rejected. ok is false for a rejected pair.
func putSisterLink(tx *sql.Tx, post, group, other int64, title string, date int64, source string, at int64) (bool, error) {
	var state string
	err := tx.QueryRow(`SELECT state FROM sister_links WHERE post_id = ? AND other_group = ? AND other_post = ?`, post, group, other).Scan(&state)
	switch {
	case err == nil && state == "active":
		_, err := tx.Exec(`UPDATE sister_links SET other_title = ?, other_date = ? WHERE post_id = ? AND other_group = ? AND other_post = ?`,
			title, date, post, group, other)
		return err == nil, err
	case err == nil:
		return false, nil
	case errors.Is(err, sql.ErrNoRows):
		_, err := tx.Exec(`INSERT INTO sister_links (post_id, other_group, other_post, source, other_title, other_date, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, post, group, other, source, title, date, at)
		return err == nil, err
	}
	return false, err
}

// RemoveSisterLink is a mod of either group taking down a cross-link. It's
// remembered as rejected on both sides, so it's never made again.
type RemoveSisterLink struct {
	GroupID    int64
	PostID     int64
	OtherGroup int64
	OtherPost  int64
	By         int64
	At         int64
}

func (c *RemoveSisterLink) Apply(a *Applier) (any, error) {
	sides := [][4]int64{{c.GroupID, c.PostID, c.OtherGroup, c.OtherPost}, {c.OtherGroup, c.OtherPost, c.GroupID, c.PostID}}
	for i, s := range sides {
		here, post, there, other := s[0], s[1], s[2], s[3]
		err := a.Group(here, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO sister_links (post_id, other_group, other_post, source, state, created_at)
				VALUES (?, ?, ?, 'mod', 'rejected', ?)
				ON CONFLICT (post_id, other_group, other_post) DO UPDATE SET state = 'rejected'`, post, there, other, c.At); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE notes SET state = 'removed', removed_by = ? WHERE kind = 'link' AND host_post_id = ?
				AND id IN (SELECT note_id FROM note_sources WHERE group_id = ? AND post_id = ?)`, c.By, post, there, other); err != nil {
				return err
			}
			if i == 0 {
				return modLog(tx, c.By, "unlink_sister", "post", post, "", c.At)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// MarkSisterStale is the worker's sister sweep noticing that the thread a
// sister note is written from has changed: the note's ext_version goes up
// to the other thread's new version and it's queued to be rewritten. (A
// thread change can't reach into another group's file itself; see the
// package note above.)
type MarkSisterStale struct {
	GroupID int64
	NoteID  int64
	Version int64
	Title   string // the other post's title now (it may have been edited)
	At      int64
}

func (c *MarkSisterStale) Apply(a *Applier) (any, error) {
	if c.Title == "" || len(c.Title) > MaxTitleLen {
		return nil, Invalid("bad title")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE sister_links SET other_title = ? WHERE (post_id, other_group, other_post) IN (
			SELECT n.host_post_id, s.group_id, s.post_id FROM notes n JOIN note_sources s ON s.note_id = n.id WHERE n.id = ?)`,
			c.Title, c.NoteID); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE notes SET ext_version = ?, stale = 1 WHERE id = ? AND ext_version < ? AND state = 'active'`,
			c.Version, c.NoteID, c.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		v, err := noteVersion(tx, c.NoteID)
		if err != nil {
			return err
		}
		return schedule(tx, JobNote, c.NoteID, v, c.At, c.At)
	})
}

// sisterRef is a post in a sister group that a post here is linked to.
type sisterRef struct{ group, post int64 }

// sisterRefs lists the sister-group posts a post is linked to.
func sisterRefs(tx *sql.Tx, postID int64) ([]sisterRef, error) {
	rows, err := tx.Query(`SELECT other_group, other_post FROM sister_links WHERE post_id = ? AND state = 'active'
		ORDER BY other_group, other_post`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sisterRef
	for rows.Next() {
		var r sisterRef
		if err := rows.Scan(&r.group, &r.post); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// showSisterNotes hides (or shows again) the notes in sister groups that
// are written from post, when it's deleted (or restored). A note's text
// is made from the post, so it must go when the post does. Only notes
// taken down this way come back on a restore, not ones a mod removed.
func showSisterNotes(a *Applier, groupID, post int64, refs []sisterRef, show bool) error {
	from, to := "active", "removed"
	if show {
		from, to = to, from
	}
	byGroup := map[int64][]any{}
	var groups []int64
	for _, r := range refs {
		if byGroup[r.group] == nil {
			groups = append(groups, r.group)
		}
		byGroup[r.group] = append(byGroup[r.group], r.post)
	}
	for _, g := range groups { // one transaction per file; refs are sorted by group
		hosts := byGroup[g]
		err := a.Group(g, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE notes SET state = ? WHERE kind = 'link' AND state = ? AND removed_by IS NULL
				AND host_post_id IN (`+placeholders(len(hosts))+`)
				AND id IN (SELECT note_id FROM note_sources WHERE group_id = ? AND post_id = ?)`,
				append(append([]any{to, from}, hosts...), groupID, post)...)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
