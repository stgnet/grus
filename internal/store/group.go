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

// settingsCols are the settings columns, the same in site.db's
// group_settings and a group file's settings row.
const settingsCols = `name, description, rules, visibility, join_policy, join_questions, allow_anonymous,
	hold_first_post, allow_indexing, ai_enabled, ai_local_only, public_faq, vote_threshold, notify_hidden`

func scanSettings(row *sql.Row) (*Settings, error) {
	var st Settings
	err := row.Scan(&st.Name, &st.Description, &st.Rules, &st.Visibility, &st.JoinPolicy, &st.JoinQuestions, &st.AllowAnonymous,
		&st.HoldFirstPost, &st.AllowIndexing, &st.AIEnabled, &st.AILocalOnly, &st.PublicFAQ, &st.VoteThreshold, &st.NotifyHidden)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GroupSettings reads a group's settings from site.db, where they live,
// so any node can read them whether or not it holds the group. A group
// made before settings moved there, whose row hasn't been copied up yet
// (cmd.ExportSettings), is read from its own file.
func (s *Store) GroupSettings(groupID int64) (*Settings, error) {
	st, err := scanSettings(s.Site().QueryRow(`SELECT `+settingsCols+` FROM group_settings WHERE group_id = ?`, groupID))
	if st != nil || err != nil {
		return st, err
	}
	return s.GroupFileSettings(groupID)
}

// GroupFileSettings reads the copy of a group's settings in its own file:
// what the group's own commands see. Use GroupSettings otherwise.
func (s *Store) GroupFileSettings(groupID int64) (*Settings, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	return scanSettings(db.QueryRow(`SELECT ` + settingsCols + ` FROM settings WHERE id = 1`))
}

// GroupsMissingSettings lists the groups whose settings are only in
// their own file so far (made before settings moved to site.db).
func (s *Store) GroupsMissingSettings() ([]int64, error) {
	rows, err := s.Site().Query(`SELECT id FROM groups WHERE id NOT IN (SELECT group_id FROM group_settings) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
