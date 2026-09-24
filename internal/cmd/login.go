package cmd

import (
	"database/sql"
	"errors"
)

// Sign-in commands. The flow (plan section 8, "From a shared link to the
// post"): CreateLogin when someone enters their email; RedeemLogin when they
// tap Continue on the emailed link or type the 6-digit code; SetHandle on
// their first sign-in.

// MaxCodeTries is how many wrong codes burn a sign-in, so the 6-digit code
// can't be guessed (a million codes, five guesses).
const MaxCodeTries = 5

// CreateLogin records an emailed sign-in. Only hashes are stored: someone
// who reads the database can't use a pending link or code.
type CreateLogin struct {
	TokenHash string
	CodeHash  string
	Email     string
	ReturnURL string // already checked against our own hosts
	At        int64
	ExpiresAt int64
}

func (c *CreateLogin) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO login_tokens (token_hash, email, code_hash, return_url, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			c.TokenHash, c.Email, c.CodeHash, c.ReturnURL, c.At, c.ExpiresAt)
		return err
	})
}

// FailLoginCode counts one wrong code. It's a replicated write (not an
// in-memory counter) so that the limit holds whichever node the guesses
// arrive at.
type FailLoginCode struct {
	TokenHash string
}

func (c *FailLoginCode) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE login_tokens SET tries = tries + 1 WHERE token_hash = ?`, c.TokenHash)
		return err
	})
}

// RedeemLogin uses up a sign-in and starts a session. It finds the account
// for the token's email, or creates one (with NewUserID) on a first sign-in.
//
// The checks (unused, unexpired, not burned) happen here, inside the log,
// rather than only in the handler: if the link and the code were both used
// at once, on two nodes, exactly one of the two RedeemLogin commands wins.
type RedeemLogin struct {
	TokenHash      string
	NewUserID      int64 // used only if this email has no account yet
	SessionHash    string
	SessionExpires int64
	UserAgentHint  string
	Operator       bool // config lists this email as an operator
	At             int64
}

// Redeemed is RedeemLogin's result.
type Redeemed struct {
	UserID    int64
	ReturnURL string
}

func (c *RedeemLogin) Apply(a *Applier) (any, error) {
	var out Redeemed
	err := a.Site(func(tx *sql.Tx) error {
		var email string
		var expires int64
		var tries int
		var used sql.NullInt64
		err := tx.QueryRow(`SELECT email, return_url, expires_at, tries, used_at FROM login_tokens WHERE token_hash = ?`,
			c.TokenHash).Scan(&email, &out.ReturnURL, &expires, &tries, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLoginDead
		}
		if err != nil {
			return err
		}
		if used.Valid || expires <= c.At || tries >= MaxCodeTries {
			return ErrLoginDead
		}
		if _, err := tx.Exec(`UPDATE login_tokens SET used_at = ? WHERE token_hash = ?`, c.At, c.TokenHash); err != nil {
			return err
		}

		// Find the account. A deleted account still waiting to be purged is
		// brought back: signing in again during the grace period is how
		// someone undoes a mistaken delete.
		err = tx.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&out.UserID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out.UserID = c.NewUserID
			if _, err := tx.Exec(`INSERT INTO users (id, email, created_at, is_operator, notify_email) VALUES (?, ?, ?, ?, 0)`,
				out.UserID, email, c.At, c.Operator); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.Exec(`UPDATE users SET deleted_at = NULL, purge_after = NULL WHERE id = ?`, out.UserID); err != nil {
				return err
			}
			if c.Operator {
				if _, err := tx.Exec(`UPDATE users SET is_operator = 1 WHERE id = ?`, out.UserID); err != nil {
					return err
				}
			}
		}

		_, err = tx.Exec(`
			INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_used_at, user_agent_hint)
			VALUES (?, ?, ?, ?, ?, ?)`,
			c.SessionHash, out.UserID, c.At, c.SessionExpires, c.At, c.UserAgentHint)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetHandle sets (or changes) an account's handle.
type SetHandle struct {
	UserID int64
	Handle string
}

func (c *SetHandle) Apply(a *Applier) (any, error) {
	if err := ValidHandle(c.Handle); err != nil {
		return nil, err
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE handle = ? AND id != ?`, c.Handle, c.UserID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrHandleTaken
		}
		res, err := tx.Exec(`UPDATE users SET handle = ? WHERE id = ? AND deleted_at IS NULL`, c.Handle, c.UserID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound(sql.ErrNoRows)
		}
		return nil
	})
}

// EndSession signs one session out, on every node.
type EndSession struct {
	TokenHash string
}

func (c *EndSession) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE token_hash = ?`, c.TokenHash)
		return err
	})
}
