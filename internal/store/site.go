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
}

// Group is a row of the site's group list. Settings live in the group's own
// file (GroupSettings).
type Group struct {
	ID        int64
	Slug      string
	MainHost  string // "" = <slug>.<primary>
	Name      string
	Status    string
	CreatedAt int64
	// Copies of the group's own settings (see the groups table).
	Visibility string
	AIEnabled  bool
}

// Domain is a row of the domains table.
type Domain struct {
	Name string
	Role string // primary | alternate
}

// LoginToken is one emailed sign-in.
type LoginToken struct {
	TokenHash string
	Email     string
	CodeHash  string
	ReturnURL string
	ExpiresAt int64
	Tries     int
	UsedAt    int64 // 0 = unused
}

// PrimaryDomain returns the current primary domain.
func (s *Store) PrimaryDomain() (string, error) {
	var name string
	err := s.Site().QueryRow(`SELECT name FROM domains WHERE role = 'primary'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// Domains lists every domain, primary first.
func (s *Store) Domains() ([]Domain, error) {
	rows, err := s.Site().Query(`SELECT name, role FROM domains ORDER BY role = 'alternate', name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Name, &d.Role); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const groupCols = `id, slug, COALESCE(main_host, ''), name, status, created_at, visibility, ai_enabled`

func scanGroup(row interface{ Scan(...any) error }) (*Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Slug, &g.MainHost, &g.Name, &g.Status, &g.CreatedAt, &g.Visibility, &g.AIEnabled)
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

// GroupByMainHost finds the group whose custom domain is host.
func (s *Store) GroupByMainHost(host string) (*Group, error) {
	return scanGroup(s.Site().QueryRow(`SELECT `+groupCols+` FROM groups WHERE main_host = ?`, host))
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

// HostAlias returns the group an alias host points at. found is false when
// host isn't an alias; groupID is 0 for an alias of the home page.
func (s *Store) HostAlias(host string) (groupID int64, found bool, err error) {
	var gid sql.NullInt64
	err = s.Site().QueryRow(`SELECT group_id FROM host_aliases WHERE host = ?`, host).Scan(&gid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return gid.Int64, true, nil
}

const userCols = `id, COALESCE(handle, ''), COALESCE(email, ''), is_operator, suspended_until, created_at,
	notify_email, digest, COALESCE(bio, ''), COALESCE(photo_key, '')`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.IsOperator, &u.SuspendedUntil, &u.CreatedAt,
		&u.NotifyEmail, &u.Digest, &u.Bio, &u.Photo)
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
		SELECT token_hash, email, code_hash, return_url, expires_at, tries, used_at
		FROM login_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&t.TokenHash, &t.Email, &t.CodeHash, &t.ReturnURL, &t.ExpiresAt, &t.Tries, &used)
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
