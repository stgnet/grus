package store

import (
	"database/sql"
	"errors"
)

// Read queries against a group's own file.

// Settings is the group's settings row.
type Settings struct {
	Name           string
	Description    string
	Rules          string
	Visibility     string // public | private | hidden
	JoinPolicy     string // open | approval | invite
	JoinQuestions  string
	AllowAnonymous bool
	HoldFirstPost  bool
	AllowIndexing  bool // search engines
	AIEnabled      bool // "Use AI in this group"
	AILocalOnly    bool // never overflow to an outside AI service (there's none in v1)
	PublicFAQ      bool // a private group's FAQ is readable by everyone
	VoteThreshold  int
	NotifyHidden   bool // tell authors when their post is hidden
}

// Membership is one user's standing in one group.
type Membership struct {
	Role        string // owner | mod | member
	Status      string // active | pending | banned
	BannedUntil int64  // 0 = permanent (when banned)
	// DigestSentAt: when this group was last in the member's daily digest.
	DigestSentAt int64
}

// GroupSettings reads a group's settings.
func (s *Store) GroupSettings(groupID int64) (*Settings, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	var st Settings
	err = db.QueryRow(`SELECT name, description, rules, visibility, join_policy, join_questions, allow_anonymous,
		hold_first_post, allow_indexing, ai_enabled, ai_local_only, public_faq, vote_threshold, notify_hidden
		FROM settings WHERE id = 1`).
		Scan(&st.Name, &st.Description, &st.Rules, &st.Visibility, &st.JoinPolicy, &st.JoinQuestions, &st.AllowAnonymous,
			&st.HoldFirstPost, &st.AllowIndexing, &st.AIEnabled, &st.AILocalOnly, &st.PublicFAQ, &st.VoteThreshold, &st.NotifyHidden)
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
	err = db.QueryRow(`SELECT role, status, banned_until, digest_sent_at FROM memberships WHERE user_id = ?`, userID).Scan(&m.Role, &m.Status, &m.BannedUntil, &m.DigestSentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
