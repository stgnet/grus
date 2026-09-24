package store

import (
	"database/sql"
	"errors"
)

// Read queries against a group's own file.

// Settings is the group's settings row (the parts M0 uses).
type Settings struct {
	Name        string
	Description string
	Rules       string
	Visibility  string // public | private | hidden
	JoinPolicy  string // open | approval | invite
}

// Membership is one user's standing in one group.
type Membership struct {
	Role   string // owner | mod | member
	Status string // active | pending | banned
}

// GroupSettings reads a group's settings.
func (s *Store) GroupSettings(groupID int64) (*Settings, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	var st Settings
	err = db.QueryRow(`SELECT name, description, rules, visibility, join_policy FROM settings WHERE id = 1`).
		Scan(&st.Name, &st.Description, &st.Rules, &st.Visibility, &st.JoinPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// Membership returns userID's membership of a group, or nil if none.
func (s *Store) Membership(groupID, userID int64) (*Membership, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	var m Membership
	err = db.QueryRow(`SELECT role, status FROM memberships WHERE user_id = ?`, userID).Scan(&m.Role, &m.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
