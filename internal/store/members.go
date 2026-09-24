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
