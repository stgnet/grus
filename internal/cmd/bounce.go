package cmd

import (
	"database/sql"
	"errors"
	"strings"
)

// A group's own domain (plan section 8, "Domains"): travatoowners.com
// instead of travato.nfb.group. The site's pages still live on the
// primary, and so does sign-in; the session cookie a browser has for the
// primary isn't sent to another domain, so the first visit there bounces
// through the primary for a one-time code (StartBounce), which the domain
// trades for its own session (FinishBounce). The visitor sees two quick
// redirects and is signed in.

// BounceTTL is how long a bounce code lasts: long enough for two
// redirects, short enough that one leaked from a log is useless.
const BounceTTL = 60

// SetGroupHost gives a group its own domain, or with Host "" takes it back
// to <slug>.<primary>. Its old address then redirects to the new one.
type SetGroupHost struct {
	GroupID int64
	Host    string
	At      int64
}

func (*SetGroupHost) siteLog() {}

func (c *SetGroupHost) Apply(a *Applier) (any, error) {
	host := strings.ToLower(strings.TrimSpace(c.Host))
	if host != "" {
		if err := ValidDomain(host); err != nil {
			return nil, err
		}
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		if host != "" {
			var primary string
			tx.QueryRow(`SELECT name FROM domains WHERE role = 'primary'`).Scan(&primary)
			if primary != "" && (host == primary || strings.HasSuffix(host, "."+primary)) {
				return Invalid("%s is already under the primary domain", host)
			}
			var n int
			tx.QueryRow(`SELECT (SELECT COUNT(*) FROM groups WHERE main_host = ?1 AND id != ?2) +
				(SELECT COUNT(*) FROM domains WHERE name = ?1) + (SELECT COUNT(*) FROM host_aliases WHERE host = ?1)`,
				host, c.GroupID).Scan(&n)
			if n > 0 {
				return Invalid("%s is already in use", host)
			}
		}
		var h any = host
		if host == "" {
			h = nil
		}
		res, err := tx.Exec(`UPDATE groups SET main_host = ? WHERE id = ?`, h, c.GroupID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// StartBounce records a one-time code (its hash) that signs UserID in on
// Host. The primary makes one for a signed-in visitor on their way there.
type StartBounce struct {
	CodeHash  string
	UserID    int64
	Host      string
	ExpiresAt int64
	At        int64
}

func (c *StartBounce) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		// Clear out old codes as we go: there are only ever a few.
		if _, err := tx.Exec(`DELETE FROM bounces WHERE expires_at <= ?`, c.At); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO bounces (code_hash, user_id, host, expires_at) VALUES (?, ?, ?, ?)`,
			c.CodeHash, c.UserID, c.Host, c.ExpiresAt)
		return err
	})
}

// FinishBounce trades a bounce code, on the host it was made for, for a
// new session there. The code works once. It returns the user's id.
type FinishBounce struct {
	CodeHash       string
	Host           string
	SessionHash    string
	SessionExpires int64
	UserAgentHint  string
	At             int64
}

func (c *FinishBounce) Apply(a *Applier) (any, error) {
	var userID int64
	err := a.Site(func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT user_id FROM bounces WHERE code_hash = ? AND host = ? AND expires_at > ?`,
			c.CodeHash, c.Host, c.At).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLoginDead
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM bounces WHERE code_hash = ?`, c.CodeHash); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_used_at, user_agent_hint)
			VALUES (?, ?, ?, ?, ?, ?)`, c.SessionHash, userID, c.At, c.SessionExpires, c.At, c.UserAgentHint)
		return err
	})
	if err != nil {
		return nil, err
	}
	return userID, nil
}
