package store

import (
	"database/sql"
	"errors"
	"strings"
)

// Read queries against site.db. Each returns (nil, nil) when the row doesn't
// exist, so callers can tell "not found" from a real error without importing
// database/sql.

// User is an account. Handle is empty until the first-sign-in screen.
type User struct {
	ID             int64
	Handle         string
	Email          string
	IsOperator     bool
	SuspendedUntil int64
	CreatedAt      int64
	NotifyEmail    bool   // notifications are also emailed (M6)
	Digest         string // off | daily
	Bio            string // M7: the public profile
	Photo          string // blob hash of the profile photo, or ""
	// Domain is the one this account last signed in on: email that isn't
	// a reply to a request is sent through it and links to it.
	Domain string
}

// Group is a row of the site's group list. Settings live in the group's own
// file (GroupSettings).
type Group struct {
	ID        int64
	Slug      string // the group is at <slug>.<domain>, on every domain
	Name      string
	Status    string
	CreatedAt int64
	// Copies of the group's own settings (see the groups table).
	Visibility string
	AIEnabled  bool
}

// Domain is a row of the domains table: one of the site's domains, all
// equal, and the mail settings for it (empty = the global ones).
type Domain struct {
	Name      string
	CreatedAt int64
	SMTPHost  string
	SMTPPort  int
	SMTPUser  string
	SMTPPass  string
	MailFrom  string
}

// Domains lists every domain, oldest first. The oldest is only a fallback:
// for email to someone who has never signed in on a domain that's still
// listed.
func (s *Store) Domains() ([]Domain, error) {
	rows, err := s.Site().Query(`SELECT name, created_at, smtp_host, smtp_port, smtp_user, smtp_pass, mail_from
		FROM domains ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Name, &d.CreatedAt, &d.SMTPHost, &d.SMTPPort, &d.SMTPUser, &d.SMTPPass, &d.MailFrom); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LoginToken is one emailed sign-in.
type LoginToken struct {
	TokenHash string
	Email     string
	CodeHash  string
	ReturnURL string
	Domain    string // the domain the sign-in was asked for on
	ExpiresAt int64
	Tries     int
	UsedAt    int64 // 0 = unused
}

// DomainNamed returns one domain's row, or nil if it isn't listed.
func (s *Store) DomainNamed(name string) (*Domain, error) {
	var d Domain
	err := s.Site().QueryRow(`SELECT name, created_at, smtp_host, smtp_port, smtp_user, smtp_pass, mail_from
		FROM domains WHERE name = ?`, name).
		Scan(&d.Name, &d.CreatedAt, &d.SMTPHost, &d.SMTPPort, &d.SMTPUser, &d.SMTPPass, &d.MailFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

const groupCols = `id, slug, name, status, created_at, visibility, ai_enabled`

func scanGroup(row interface{ Scan(...any) error }) (*Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Slug, &g.Name, &g.Status, &g.CreatedAt, &g.Visibility, &g.AIEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// GroupByID looks a group up by id.
func (s *Store) GroupByID(id int64) (*Group, error) {
	return scanGroup(s.Site().QueryRow(`SELECT `+groupCols+` FROM groups WHERE id = ?`, id))
}

// GroupBySlug looks a group up by slug.
func (s *Store) GroupBySlug(slug string) (*Group, error) {
	return scanGroup(s.Site().QueryRow(`SELECT `+groupCols+` FROM groups WHERE slug = ?`, slug))
}

// Groups lists every group by name.
func (s *Store) Groups() ([]Group, error) {
	rows, err := s.Site().Query(`SELECT ` + groupCols + ` FROM groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

const userCols = `id, COALESCE(handle, ''), COALESCE(email, ''), is_operator, suspended_until, created_at,
	notify_email, digest, COALESCE(bio, ''), COALESCE(photo_key, ''), domain`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.IsOperator, &u.SuspendedUntil, &u.CreatedAt,
		&u.NotifyEmail, &u.Digest, &u.Bio, &u.Photo, &u.Domain)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UserByID looks up a live (not deleted) account.
func (s *Store) UserByID(id int64) (*User, error) {
	return scanUser(s.Site().QueryRow(`SELECT `+userCols+` FROM users WHERE id = ? AND deleted_at IS NULL`, id))
}

// UserByHandle looks up a live account by its handle, ignoring case.
func (s *Store) UserByHandle(handle string) (*User, error) {
	return scanUser(s.Site().QueryRow(`SELECT `+userCols+` FROM users
		WHERE handle = ? COLLATE NOCASE AND deleted_at IS NULL`, handle))
}

// UserByBlob finds a live account whose profile photo is hash.
func (s *Store) UserByBlob(hash string) (*User, error) {
	return scanUser(s.Site().QueryRow(`SELECT `+userCols+` FROM users
		WHERE photo_key = ? AND deleted_at IS NULL LIMIT 1`, hash))
}

// UserBySession returns the account a session token (hashed) belongs to, if
// the session hasn't expired at time now.
func (s *Store) UserBySession(tokenHash string, now int64) (*User, error) {
	return scanUser(s.Site().QueryRow(`
		SELECT `+userCols+` FROM users
		WHERE deleted_at IS NULL AND id = (
			SELECT user_id FROM sessions WHERE token_hash = ? AND expires_at > ?)`,
		tokenHash, now))
}

// HandleTaken reports whether any account (other than exceptID) uses handle.
func (s *Store) HandleTaken(handle string, exceptID int64) (bool, error) {
	var n int
	err := s.Site().QueryRow(`SELECT COUNT(*) FROM users WHERE handle = ? AND id != ?`, handle, exceptID).Scan(&n)
	return n > 0, err
}

// LoginToken looks up an emailed sign-in by its hash.
func (s *Store) LoginToken(tokenHash string) (*LoginToken, error) {
	var t LoginToken
	var used sql.NullInt64
	err := s.Site().QueryRow(`
		SELECT token_hash, email, code_hash, return_url, domain, expires_at, tries, used_at
		FROM login_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&t.TokenHash, &t.Email, &t.CodeHash, &t.ReturnURL, &t.Domain, &t.ExpiresAt, &t.Tries, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.UsedAt = used.Int64
	return &t, nil
}

// Cert returns a cached certificate (or the ACME account key) by name.
func (s *Store) Cert(name string) ([]byte, error) {
	var pem []byte
	err := s.Site().QueryRow(`SELECT pem FROM certs WHERE domain = ?`, name).Scan(&pem)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return pem, err
}

// UserByName finds an account by its handle, or (for the operator's
// tools) its email. nil if there's none.
func (s *Store) UserByName(name string) (*User, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	return scanUser(s.Site().QueryRow(`SELECT `+userCols+` FROM users WHERE (handle = ?1 COLLATE NOCASE OR email = lower(?1)) AND deleted_at IS NULL`, name))
}
