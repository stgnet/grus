package cmd

import (
	"database/sql"
	"errors"
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
// The pairing commands are on the site log. What they change in the
// groups' own files (the mod log lines, taking links down) is sent to each
// group's log (see logs.go). A check that links two posts writes its own
// side and sends the other side the same way. What a check needs to know
// about the other group (visibility, "Use AI", whether the pair is active)
// is looked up by the worker in site.db and carried in the command.

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
		case err == nil || errors.Is(err, sql.ErrNoRows):
			state = "proposed"
			_, err = tx.Exec(`INSERT INTO group_pairs (group_a, group_b, proposed_by_group, topics, state, proposed_by, created_at, updated_at)
				VALUES (?, ?, ?, ?, 'proposed', ?, ?, ?)
				ON CONFLICT (group_a, group_b) DO UPDATE SET proposed_by_group = excluded.proposed_by_group, topics = excluded.topics,
				  state = 'proposed', proposed_by = excluded.proposed_by, decided_by = NULL, updated_at = excluded.updated_at`,
				ga, gb, c.GroupID, topics, c.By, c.At, c.At)
		}
		if err != nil {
			return err
		}
		return send(tx, &ModLogEntry{GroupID: c.GroupID, By: c.By, Action: "sister_" + state, TargetType: "group",
			TargetID: c.Other, Reason: topics, At: c.At}, c.At)
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (*ProposeSister) siteLog() {}

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
		return send(tx, &ModLogEntry{GroupID: c.GroupID, By: c.By, Action: "sister_" + map[bool]string{true: "accept", false: "decline"}[c.Accept],
			TargetType: "group", TargetID: c.Other, At: c.At}, c.At)
	})
	return nil, err
}

func (*AnswerSister) siteLog() {}

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
	return nil, a.Site(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE group_pairs SET state = 'ended', decided_by = ?, updated_at = ? WHERE group_a = ? AND group_b = ?`,
			c.By, c.At, ga, gb); err != nil {
			return err
		}
		// Each side's links and notes to the other come down.
		if err := send(tx, &DropSisterLinks{GroupID: c.GroupID, Other: c.Other, By: c.By, At: c.At}, c.At); err != nil {
			return err
		}
		return send(tx, &DropSisterLinks{GroupID: c.Other, Other: c.GroupID, At: c.At}, c.At)
	})
}

func (*EndSister) siteLog() {}

// SisterMatch is one post in a sister group that a check job found to be
// about the same thing: the note ids to use on each side, the other
// thread's version as the worker read it, and which sides may carry a
// note (from SisterRule, looked up by the worker when it read site.db).
type SisterMatch struct {
	Group     int64
	Post      int64
	Version   int64
	Title     string // the other post's title and date, for the note's heading
	Date      int64
	NoteHere  int64
	NoteThere int64
	CiteHere  bool // a note on this post, citing the other
	CiteThere bool // a note on the other post, citing this one
}

// SisterRule says whether posts in two groups may be linked, and which
// side may show a note citing the other: the pairing must be active, both
// groups must have AI on, and a note may only cite a public group's post
// (auth.CanCite). It reads site.db, so the worker calls it and puts the
// answer in the command: a group's command can't read site.db itself
// (logs.go). The rule is checked again when a note is shown, so a group
// changing its visibility later takes effect at once.
func SisterRule(site *sql.DB, here, there int64) (link, citeHere, citeThere bool, err error) {
	visHere, aiHere, err := groupFacts(site, here)
	if err != nil || !aiHere {
		return false, false, false, nil
	}
	p, err := sisterPair(site, here, there)
	if err != nil {
		return false, false, false, err
	}
	visThere, aiThere, err := groupFacts(site, there)
	if err != nil || !p.Active || !aiThere {
		return false, false, false, nil
	}
	citeHere, citeThere = auth.CanCite(visThere), auth.CanCite(visHere)
	return citeHere || citeThere, citeHere, citeThere, nil
}

// sisterHere writes this group's side of a check's sister links: the link
// rows and notes on this post. It returns the matches a mod hasn't
// rejected, whose other sides sendSisterSides then sends.
func sisterHere(tx *sql.Tx, postID int64, matches []SisterMatch, at int64) ([]SisterMatch, error) {
	var kept []SisterMatch
	for _, m := range matches {
		if !m.CiteHere && !m.CiteThere {
			continue
		}
		ok, err := putSisterLink(tx, postID, m.Group, m.Post, m.Title, m.Date, "auto", at)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a mod rejected this pair
		}
		kept = append(kept, m)
		if m.CiteHere {
			if err := linkNote(tx, m.Group, postID, m.Post, m.NoteHere, m.Version, at); err != nil {
				return nil, err
			}
		}
	}
	return kept, nil
}

// sendSisterSides sends each sister group its side of the new links.
// title, date and version are this post's, as of this command.
func sendSisterSides(tx *sql.Tx, groupID, postID int64, title string, date, version int64, kept []SisterMatch, at int64) error {
	for _, m := range kept {
		err := send(tx, &PutSisterSide{GroupID: m.Group, Post: m.Post, FromGroup: groupID, FromPost: postID,
			Title: title, Date: date, Version: version, NoteID: m.NoteThere, Cite: m.CiteThere, At: at}, at)
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
// remembered as rejected on both sides, so it's never made again: this
// side here, and the other side by the same command sent there (Echo).
type RemoveSisterLink struct {
	GroupID    int64
	PostID     int64
	OtherGroup int64
	OtherPost  int64
	By         int64
	Echo       bool // the other side's copy: no mod log line, nothing sent on
	At         int64
}

func (c *RemoveSisterLink) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sister_links (post_id, other_group, other_post, source, state, created_at)
			VALUES (?, ?, ?, 'mod', 'rejected', ?)
			ON CONFLICT (post_id, other_group, other_post) DO UPDATE SET state = 'rejected'`, c.PostID, c.OtherGroup, c.OtherPost, c.At); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE notes SET state = 'removed', removed_by = ? WHERE kind = 'link' AND host_post_id = ?
			AND id IN (SELECT note_id FROM note_sources WHERE group_id = ? AND post_id = ?)`, c.By, c.PostID, c.OtherGroup, c.OtherPost); err != nil {
			return err
		}
		if c.Echo {
			return nil
		}
		if err := modLog(tx, c.By, "unlink_sister", "post", c.PostID, "", c.At); err != nil {
			return err
		}
		return send(tx, &RemoveSisterLink{GroupID: c.OtherGroup, PostID: c.OtherPost, OtherGroup: c.GroupID, OtherPost: c.PostID,
			By: c.By, Echo: true, At: c.At}, c.At)
	})
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
