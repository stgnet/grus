package store

import (
	"database/sql"
	"errors"
)

// Join requests, invites, and sister groups (M4).

// JoinRequest is someone waiting for a mod to let them in.
type JoinRequest struct {
	UserID    int64
	Answers   string
	CreatedAt int64
}

// JoinRequests lists a group's pending join requests, oldest first.
func (s *Store) JoinRequests(groupID int64) ([]JoinRequest, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT user_id, join_answers, created_at FROM memberships WHERE status = 'pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JoinRequest
	for rows.Next() {
		var r JoinRequest
		if err := rows.Scan(&r.UserID, &r.Answers, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Invite is an invite link.
type Invite struct {
	Code      string
	CreatedBy int64
	MaxUses   int
	Uses      int
	ExpiresAt int64
	Revoked   bool
	CreatedAt int64
}

// Usable: can this invite still let someone in at time now?
func (i *Invite) Usable(now int64) bool {
	return !i.Revoked && i.Uses < i.MaxUses && i.ExpiresAt > now
}

const inviteCols = `code, created_by, max_uses, uses, expires_at, revoked, created_at`

func scanInvite(row interface{ Scan(...any) error }) (*Invite, error) {
	var i Invite
	err := row.Scan(&i.Code, &i.CreatedBy, &i.MaxUses, &i.Uses, &i.ExpiresAt, &i.Revoked, &i.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// Invite looks up an invite by its code.
func (s *Store) Invite(groupID int64, code string) (*Invite, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	return scanInvite(db.QueryRow(`SELECT `+inviteCols+` FROM invites WHERE code = ?`, code))
}

// Invites lists a group's invites that still work, newest first.
func (s *Store) Invites(groupID, now int64) ([]Invite, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+inviteCols+` FROM invites WHERE revoked = 0 AND uses < max_uses AND expires_at > ?
		ORDER BY created_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// Pair is a group's pairing with another group, from its side.
type Pair struct {
	Other    int64
	State    string // proposed | active | ended
	Topics   string
	Incoming bool // proposed by the other group, waiting on this one
}

// Pairs lists a group's pairings that are proposed or active.
func (s *Store) Pairs(groupID int64) ([]Pair, error) {
	rows, err := s.Site().Query(`SELECT CASE WHEN group_a = ?1 THEN group_b ELSE group_a END, state, topics, proposed_by_group != ?1
		FROM group_pairs WHERE (group_a = ?1 OR group_b = ?1) AND state IN ('proposed', 'active') ORDER BY updated_at`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pair
	for rows.Next() {
		var p Pair
		if err := rows.Scan(&p.Other, &p.State, &p.Topics, &p.Incoming); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Sisters lists a group's active sister groups.
func (s *Store) Sisters(groupID int64) ([]Pair, error) {
	all, err := s.Pairs(groupID)
	if err != nil {
		return nil, err
	}
	var out []Pair
	for _, p := range all {
		if p.State == "active" {
			out = append(out, p)
		}
	}
	return out, nil
}

// SisterLink is a link from a post here to a post in a sister group.
type SisterLink struct {
	OtherGroup int64
	OtherPost  int64
	Title      string
	Date       int64
	State      string
}

// SisterLinks lists a post's links to sister groups, rejected ones
// included (so the check job doesn't offer them again).
func (s *Store) SisterLinks(groupID, postID int64) ([]SisterLink, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT other_group, other_post, other_title, other_date, state FROM sister_links WHERE post_id = ?`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SisterLink
	for rows.Next() {
		var l SisterLink
		if err := rows.Scan(&l.OtherGroup, &l.OtherPost, &l.Title, &l.Date, &l.State); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SisterNote is an active note here written from a post in another group,
// for the worker's sweep.
type SisterNote struct {
	NoteID     int64
	HostPostID int64
	OtherGroup int64
	OtherPost  int64
	ExtVersion int64
}

// SisterNotes lists a group's active notes written from other groups.
func (s *Store) SisterNotes(groupID int64) ([]SisterNote, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT n.id, n.host_post_id, s.group_id, s.post_id, n.ext_version
		FROM notes n JOIN note_sources s ON s.note_id = n.id
		WHERE n.kind = 'link' AND n.state = 'active' AND s.group_id != ? ORDER BY n.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SisterNote
	for rows.Next() {
		var n SisterNote
		if err := rows.Scan(&n.NoteID, &n.HostPostID, &n.OtherGroup, &n.OtherPost, &n.ExtVersion); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ModExample is a mod's decision kept as an example for the AI check.
type ModExample struct {
	Text     string
	Decision string // keep | hide
}

// ModExamples lists a group's newest mod decisions.
func (s *Store) ModExamples(groupID int64, n int) ([]ModExample, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT text, decision FROM mod_examples ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModExample
	for rows.Next() {
		var x ModExample
		if err := rows.Scan(&x.Text, &x.Decision); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Member is one row of a group's member list.
type Member struct {
	UserID      int64
	Role        string
	Status      string
	BannedUntil int64
	CreatedAt   int64
}

// Members lists a group's members (active and banned), owners and mods
// first, then newest first.
func (s *Store) Members(groupID int64, limit int) ([]Member, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT user_id, role, status, banned_until, created_at FROM memberships WHERE status != 'pending'
		ORDER BY status = 'banned', CASE role WHEN 'owner' THEN 0 WHEN 'mod' THEN 1 ELSE 2 END, created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Role, &m.Status, &m.BannedUntil, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueueItem is a post or comment waiting on the mods: held, hidden by the
// AI or a vote, flagged, or reported.
type QueueItem struct {
	Kind    string // post | comment
	ID      int64
	PostID  int64
	Title   string // the post's title
	Body    string
	Status  string
	UserID  int64
	Flag    Flag
	Reports int
	Reasons string // the reports' reasons, joined
	At      int64
}

// ModQueue lists what's waiting on the mods, oldest first.
func (s *Store) ModQueue(groupID int64) ([]QueueItem, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		WITH open AS (SELECT kind, item_id, COUNT(*) AS n, GROUP_CONCAT(NULLIF(reason, ''), ' / ') AS reasons, MIN(created_at) AS at
		              FROM reports WHERE resolved_at IS NULL GROUP BY kind, item_id)
		SELECT 'post', p.id, p.id, p.title, p.body, p.status, COALESCE(p.user_id, 0),
		       COALESCE(p.flagged_by, ''), COALESCE(p.flag_category, ''), COALESCE(p.flag_reason, ''),
		       COALESCE(o.n, 0), COALESCE(o.reasons, ''), COALESCE(o.at, p.created_at)
		FROM posts p LEFT JOIN open o ON o.kind = 'post' AND o.item_id = p.id
		WHERE p.status IN ('held', 'auto_hidden', 'flagged') OR (o.n > 0 AND p.status NOT IN ('removed', 'deleted'))
		UNION ALL
		SELECT 'comment', c.id, c.post_id, p.title, c.body, c.status, COALESCE(c.user_id, 0),
		       COALESCE(c.flagged_by, ''), COALESCE(c.flag_category, ''), COALESCE(c.flag_reason, ''),
		       COALESCE(o.n, 0), COALESCE(o.reasons, ''), COALESCE(o.at, c.created_at)
		FROM comments c JOIN posts p ON p.id = c.post_id LEFT JOIN open o ON o.kind = 'comment' AND o.item_id = c.id
		WHERE c.status IN ('held', 'auto_hidden', 'flagged') OR (o.n > 0 AND c.status NOT IN ('removed', 'deleted'))
		ORDER BY 13 LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var q QueueItem
		if err := rows.Scan(&q.Kind, &q.ID, &q.PostID, &q.Title, &q.Body, &q.Status, &q.UserID,
			&q.Flag.By, &q.Flag.Category, &q.Flag.Reason, &q.Reports, &q.Reasons, &q.At); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// LogEntry is one mod log row. ActorID 0 = automatic (the AI check, or a
// member vote).
type LogEntry struct {
	ActorID    int64
	Action     string
	TargetType string
	TargetID   int64
	Reason     string
	At         int64
}

// ModLog lists a group's mod log, newest first.
func (s *Store) ModLog(groupID int64, limit, offset int) ([]LogEntry, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT COALESCE(actor_id, 0), action, target_type, target_id, reason, created_at
		FROM mod_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ActorID, &e.Action, &e.TargetType, &e.TargetID, &e.Reason, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ItemVotes counts Keep and Hide votes on a flagged item, and what userID
// voted ("" if they haven't).
func (s *Store) ItemVotes(groupID int64, kind string, id, userID int64) (keeps, hides int, mine string, err error) {
	db, err := s.Group(groupID)
	if err != nil {
		return 0, 0, "", err
	}
	err = db.QueryRow(`SELECT COALESCE(SUM(vote = 'keep'), 0), COALESCE(SUM(vote = 'hide'), 0),
		COALESCE(MAX(CASE WHEN user_id = ? THEN vote END), '') FROM flag_votes WHERE kind = ? AND item_id = ?`,
		userID, kind, id).Scan(&keeps, &hides, &mine)
	return
}

// HasShownContent: has userID anything shown (visible or flagged) in the
// group? New accounts without it get the new-account limits.
func (s *Store) HasShownContent(groupID, userID int64) (bool, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return false, err
	}
	var n int
	err = db.QueryRow(`SELECT EXISTS (SELECT 1 FROM posts WHERE user_id = ?1 AND status IN ('visible', 'flagged'))
		OR EXISTS (SELECT 1 FROM comments WHERE user_id = ?1 AND status IN ('visible', 'flagged'))`, userID).Scan(&n)
	return n > 0, err
}
