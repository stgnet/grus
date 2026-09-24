package cmd

import (
	"database/sql"
	"errors"
	"fmt"
)

// SetPrimaryDomain makes Domain the primary. The old primary becomes an
// alternate, which means every old link (bare or <slug>.old) redirects to the
// same place on the new primary. That's the whole "change the primary later"
// runbook as far as the data goes (plan section 8, "Domains").
type SetPrimaryDomain struct {
	Domain string
	At     int64
}

func (c *SetPrimaryDomain) Apply(a *Applier) (any, error) {
	if err := ValidDomain(c.Domain); err != nil {
		return nil, err
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		// Demote first: the unique index allows only one primary at a time.
		if _, err := tx.Exec(`UPDATE domains SET role = 'alternate' WHERE role = 'primary' AND name != ?`, c.Domain); err != nil {
			return err
		}
		_, err := tx.Exec(`
			INSERT INTO domains (name, role, created_at) VALUES (?, 'primary', ?)
			ON CONFLICT (name) DO UPDATE SET role = 'primary'`, c.Domain, c.At)
		return err
	})
}

// AddAlternateDomain adds a domain that redirects to the primary: the bare
// name to the home page, <slug>.<alternate> to <slug>.<primary>.
type AddAlternateDomain struct {
	Domain string
	At     int64
}

func (c *AddAlternateDomain) Apply(a *Applier) (any, error) {
	if err := ValidDomain(c.Domain); err != nil {
		return nil, err
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO domains (name, role, created_at) VALUES (?, 'alternate', ?)
			ON CONFLICT (name) DO NOTHING`, c.Domain, c.At)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%s is already listed", c.Domain)
		}
		return nil
	})
}

// AddHostAlias makes one extra host 301 to a group's main address (or to
// the home page when GroupID is 0).
type AddHostAlias struct {
	Host    string
	GroupID int64
	At      int64
}

func (c *AddHostAlias) Apply(a *Applier) (any, error) {
	if err := ValidDomain(c.Host); err != nil {
		return nil, err
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		var gid any // NULL for a home-page alias
		if c.GroupID != 0 {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM groups WHERE id = ?`, c.GroupID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrNotFound
			}
			gid = c.GroupID
		}
		_, err := tx.Exec(`INSERT INTO host_aliases (host, group_id, created_at) VALUES (?, ?, ?)`, c.Host, gid, c.At)
		return err
	})
}

// CreateGroup adds a group to the site's list and creates its own database
// file with its settings. OwnerID (if set) becomes the group's first owner.
type CreateGroup struct {
	GroupID     int64
	Slug        string
	Name        string
	Description string
	Visibility  string // "" = public
	OwnerID     int64
	At          int64
}

func (c *CreateGroup) Apply(a *Applier) (any, error) {
	if err := ValidSlug(c.Slug); err != nil {
		return nil, err
	}
	if c.Name == "" {
		return nil, fmt.Errorf("a group needs a name")
	}
	vis := c.Visibility
	if vis == "" {
		vis = "public"
	}
	if vis != "public" && vis != "private" && vis != "hidden" {
		return nil, Invalid("visibility must be public, private or hidden")
	}
	err := a.Site(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM groups WHERE slug = ?`, c.Slug).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSlugTaken
		}
		_, err := tx.Exec(`INSERT INTO groups (id, slug, name, visibility, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.GroupID, c.Slug, c.Name, vis, c.At)
		return err
	})
	if err != nil {
		return nil, err
	}
	// A second transaction, on the group's own file. If the node stops
	// between the two, replaying this entry skips the part site.db already
	// has and does this part (see Applier).
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		// A private group's FAQ starts private too.
		if _, err := tx.Exec(`INSERT INTO settings (id, name, description, visibility, public_faq) VALUES (1, ?, ?, ?, ?)`,
			c.Name, c.Description, vis, vis == "public"); err != nil {
			return err
		}
		if c.OwnerID != 0 {
			_, err := tx.Exec(`INSERT INTO memberships (user_id, role, status, created_at) VALUES (?, 'owner', 'active', ?)`,
				c.OwnerID, c.At)
			return err
		}
		return nil
	})
}

// PutCert stores a certificate or the ACME account key (autocert's cache).
type PutCert struct {
	Name string
	PEM  []byte
	At   int64
}

func (c *PutCert) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO certs (domain, pem, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (domain) DO UPDATE SET pem = excluded.pem, updated_at = excluded.updated_at`,
			c.Name, c.PEM, c.At)
		return err
	})
}

// DeleteCert removes a cached certificate.
type DeleteCert struct {
	Name string
}

func (c *DeleteCert) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM certs WHERE domain = ?`, c.Name)
		return err
	})
}

// notFound turns sql.ErrNoRows into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
