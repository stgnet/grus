package cmd

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
)

// Joining a group, invites, and anonymous posting (plan section 2,
// "Reading and joining", and section 6).
//
// A group's join policy is one of:
//
//	open      one tap, and you're in
//	approval  answer the group's questions; a mod approves
//	invite    only with an invite link
//
// An invite works under any policy, and is the only way into a hidden
// group (which doesn't exist for anyone else).

// Limits.
const (
	MaxJoinAnswers = 2000
	MaxInviteUses  = 1000
	MaxInviteDays  = 90
)

// inviteCodeRE: invite codes are made by the web layer from crypto/rand
// (never inside Apply). The check here only keeps junk out of the table.
var inviteCodeRE = regexp.MustCompile(`^[A-Za-z0-9]{8,40}$`)

// JoinGroup adds a member. Invite, if set, is an invite code, which makes
// them active at once whatever the policy.
type JoinGroup struct {
	GroupID int64
	UserID  int64
	Answers string
	Invite  string
	At      int64
}

// Apply returns the membership status: "active" or "pending".
func (c *JoinGroup) Apply(a *Applier) (any, error) {
	answers := strings.TrimSpace(c.Answers)
	if len(answers) > MaxJoinAnswers {
		return nil, Invalid("answers can be up to %d characters", MaxJoinAnswers)
	}
	var status string
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		var existing string
		var bannedUntil int64
		err := tx.QueryRow(`SELECT status, banned_until FROM memberships WHERE user_id = ?`, c.UserID).Scan(&existing, &bannedUntil)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		have := err == nil
		if existing == "banned" {
			if bannedUntil == 0 || bannedUntil > c.At {
				return Invalid("you can't join this group")
			}
			existing = "" // a temporary ban that has run out: join as anyone would
		}
		if existing == "active" {
			status = existing
			return nil // already a member
		}
		invited := false
		if c.Invite != "" {
			ok, err := useInvite(tx, c.Invite, c.At)
			if err != nil {
				return err
			}
			if !ok {
				return Invalid("that invite link has expired or been used up")
			}
			invited = true
		}
		var policy, questions string
		if err := tx.QueryRow(`SELECT join_policy, join_questions FROM settings WHERE id = 1`).Scan(&policy, &questions); err != nil {
			return err
		}
		switch {
		case invited || policy == "open":
			status = "active"
		case existing == "pending":
			status = existing
			return nil // already waiting
		case policy == "invite":
			return Invalid("this group is by invitation only")
		default: // approval
			if strings.TrimSpace(questions) != "" && answers == "" {
				return Invalid("please answer the group's questions")
			}
			status = "pending"
		}
		if have {
			_, err = tx.Exec(`UPDATE memberships SET status = ?1, banned_until = 0,
				join_answers = CASE WHEN ?2 = '' THEN join_answers ELSE ?2 END WHERE user_id = ?3`, status, answers, c.UserID)
			return err
		}
		_, err = tx.Exec(`INSERT INTO memberships (user_id, role, status, join_answers, created_at) VALUES (?, 'member', ?, ?, ?)`,
			c.UserID, status, answers, c.At)
		return err
	})
	return status, err
}

// useInvite counts one use of an invite, if it's still good.
func useInvite(tx *sql.Tx, code string, at int64) (bool, error) {
	res, err := tx.Exec(`UPDATE invites SET uses = uses + 1
		WHERE code = ? AND revoked = 0 AND uses < max_uses AND expires_at > ?`, code, at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReviewJoin is a mod's answer to a join request: approve makes them a
// member; decline removes the request (they can ask again later; a ban,
// M5, is how a mod stops that).
type ReviewJoin struct {
	GroupID int64
	UserID  int64
	Approve bool
	By      int64
	At      int64
}

func (c *ReviewJoin) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM memberships WHERE user_id = ?`, c.UserID).Scan(&status); err != nil || status != "pending" {
			return Invalid("that request has already been answered")
		}
		action := "approve_join"
		if c.Approve {
			if _, err := tx.Exec(`UPDATE memberships SET status = 'active' WHERE user_id = ?`, c.UserID); err != nil {
				return err
			}
			if err := notify(tx, c.UserID, NoteJoined, 0, 0, c.By, c.At); err != nil {
				return err
			}
		} else {
			action = "decline_join"
			if _, err := tx.Exec(`DELETE FROM memberships WHERE user_id = ?`, c.UserID); err != nil {
				return err
			}
		}
		return modLog(tx, c.By, action, "user", c.UserID, "", c.At)
	})
}

// CreateInvite makes an invite link. Code is chosen by the web layer.
type CreateInvite struct {
	GroupID   int64
	Code      string
	MaxUses   int
	ExpiresAt int64
	By        int64
	At        int64
}

func (c *CreateInvite) Apply(a *Applier) (any, error) {
	if !inviteCodeRE.MatchString(c.Code) {
		return nil, Invalid("bad invite code")
	}
	if c.MaxUses < 1 || c.MaxUses > MaxInviteUses {
		return nil, Invalid("an invite can be used 1 to %d times", MaxInviteUses)
	}
	if c.ExpiresAt <= c.At || c.ExpiresAt > c.At+MaxInviteDays*86400 {
		return nil, Invalid("an invite lasts 1 to %d days", MaxInviteDays)
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO invites (code, created_by, max_uses, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.Code, c.By, c.MaxUses, c.ExpiresAt, c.At); err != nil {
			return err
		}
		return modLog(tx, c.By, "create_invite", "invite", 0, "", c.At)
	})
}

// RevokeInvite stops an invite link working.
type RevokeInvite struct {
	GroupID int64
	Code    string
	By      int64
	At      int64
}

func (c *RevokeInvite) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE invites SET revoked = 1 WHERE code = ?`, c.Code); err != nil {
			return err
		}
		return modLog(tx, c.By, "revoke_invite", "invite", 0, "", c.At)
	})
}

// LeaveGroup ends someone's own membership (or withdraws their request).
// A group's last owner can't leave: someone has to be able to run it.
// A ban stays in place: leaving isn't a way around one.
type LeaveGroup struct {
	GroupID int64
	UserID  int64
	At      int64
}

func (c *LeaveGroup) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var role, status string
		if err := tx.QueryRow(`SELECT role, status FROM memberships WHERE user_id = ?`, c.UserID).Scan(&role, &status); err != nil {
			return nil // not a member; nothing to do
		}
		if status == "banned" {
			return nil
		}
		if role == "owner" {
			var owners int
			tx.QueryRow(`SELECT COUNT(*) FROM memberships WHERE role = 'owner' AND status = 'active'`).Scan(&owners)
			if owners <= 1 {
				return Invalid("you're this group's only owner; make someone else an owner first")
			}
		}
		_, err := tx.Exec(`DELETE FROM memberships WHERE user_id = ?`, c.UserID)
		return err
	})
}

// anonymousAllowed fails an anonymous post or comment in a group that
// doesn't allow them. The form only offers the box when the group does;
// this is the rule itself.
func anonymousAllowed(tx *sql.Tx, anonymous bool) error {
	if !anonymous {
		return nil
	}
	var allowed bool
	if err := tx.QueryRow(`SELECT allow_anonymous FROM settings WHERE id = 1`).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return Invalid("this group doesn't allow anonymous posts")
	}
	return nil
}

// RevealAuthor is a mod looking up who wrote an anonymous post or comment
// (plan section 2: anonymity is from other members, not from the group's
// mods when they need to act). A read, but a replicated command, because
// every reveal must go in the mod log with its reason. It returns the
// author's user id.
type RevealAuthor struct {
	GroupID int64
	Kind    string // post | comment
	ID      int64
	By      int64
	Reason  string
	At      int64
}

func (c *RevealAuthor) Apply(a *Applier) (any, error) {
	reason := strings.TrimSpace(c.Reason)
	if reason == "" || len(reason) > 500 {
		return nil, Invalid("say why (up to 500 characters); it goes in the mod log")
	}
	table := map[string]string{"post": "posts", "comment": "comments"}[c.Kind]
	if table == "" {
		return nil, Invalid("unknown kind")
	}
	var author int64
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		var uid sql.NullInt64
		var anon bool
		if err := tx.QueryRow(`SELECT user_id, is_anonymous FROM `+table+` WHERE id = ?`, c.ID).Scan(&uid, &anon); err != nil {
			return notFoundGone(err)
		}
		if !anon {
			return Invalid("that isn't anonymous")
		}
		author = uid.Int64
		return modLog(tx, c.By, "reveal_author", c.Kind, c.ID, reason, c.At)
	})
	return author, err
}
