package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Domains (see the domains table in internal/store/schema.go): one list
// for the whole site, every domain on it equal. Any node answers any of
// them, and each request is answered in the domain it came in on.

// AddDomain lists a domain. Point its DNS at the nodes first; its
// certificate is fetched the first time it's used.
type AddDomain struct {
	Domain string
	At     int64
}

func (c *AddDomain) Apply(a *Applier) (any, error) {
	if err := ValidDomain(c.Domain); err != nil {
		return nil, err
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO domains (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, c.Domain, c.At)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Invalid("%s is already listed", c.Domain)
		}
		return nil
	})
}

// RemoveDomain takes a domain off the list. Requests for it are then
// answered "no such group", and no certificate is fetched for it. The
// last domain can't be removed: the site would have no address at all.
type RemoveDomain struct {
	Domain string
}

func (c *RemoveDomain) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM domains WHERE name != ?`, c.Domain).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return Invalid("the site needs at least one domain")
		}
		res, err := tx.Exec(`DELETE FROM domains WHERE name = ?`, c.Domain)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetDomainMail sets a domain's own mail settings. An empty SMTPHost
// means the domain uses the global SMTP settings; an empty MailFrom means
// login@<domain>.
type SetDomainMail struct {
	Domain   string
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	MailFrom string
}

func (c *SetDomainMail) Apply(a *Applier) (any, error) {
	if c.SMTPPort < 0 || c.SMTPPort > 65535 {
		return nil, Invalid("smtp port must be 0 to 65535")
	}
	if strings.ContainsAny(c.SMTPHost+c.SMTPUser+c.MailFrom, "\r\n") {
		return nil, Invalid("line break in a mail setting")
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE domains SET smtp_host = ?, smtp_port = ?, smtp_user = ?, smtp_pass = ?, mail_from = ? WHERE name = ?`,
			strings.TrimSpace(c.SMTPHost), c.SMTPPort, strings.TrimSpace(c.SMTPUser), c.SMTPPass, strings.TrimSpace(c.MailFrom), c.Domain)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Before the domain list, there was a primary domain with alternates that
// redirected to it, single hosts that redirected, and groups on their own
// domains. Old logs still hold those commands, so they still decode. The
// two that listed a domain now just list it (a replay leaves the same
// list); the rest did nothing that exists any more.

// SetPrimaryDomain is the old way to list a domain.
type SetPrimaryDomain struct {
	Domain string
	At     int64
}

func (c *SetPrimaryDomain) Apply(a *Applier) (any, error) { return nil, listDomain(a, c.Domain, c.At) }

// AddAlternateDomain is the old way to list a domain.
type AddAlternateDomain struct {
	Domain string
	At     int64
}

func (c *AddAlternateDomain) Apply(a *Applier) (any, error) {
	return nil, listDomain(a, c.Domain, c.At)
}

func listDomain(a *Applier, domain string, at int64) error {
	if ValidDomain(domain) != nil {
		return nil // was refused when it was first applied, too
	}
	return a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO domains (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, domain, at)
		return err
	})
}

// AddHostAlias is gone (a host that redirected to a group); it does nothing.
type AddHostAlias struct {
	Host    string
	GroupID int64
	At      int64
}

func (*AddHostAlias) siteLog()                    {}
func (*AddHostAlias) Apply(*Applier) (any, error) { return nil, nil }

// SetGroupHost is gone (a group's own domain); it does nothing.
type SetGroupHost struct {
	GroupID int64
	Host    string
	At      int64
}

func (*SetGroupHost) siteLog()                    {}
func (*SetGroupHost) Apply(*Applier) (any, error) { return nil, nil }

// StartBounce is gone (sign-in carried to a group's own domain); it does
// nothing.
type StartBounce struct {
	CodeHash  string
	UserID    int64
	Host      string
	ExpiresAt int64
	At        int64
}

func (*StartBounce) Apply(*Applier) (any, error) { return nil, nil }

// FinishBounce is gone, like StartBounce; it does nothing.
type FinishBounce struct {
	CodeHash       string
	Host           string
	SessionHash    string
	SessionExpires int64
	UserAgentHint  string
	At             int64
}

func (*FinishBounce) Apply(*Applier) (any, error) { return nil, nil }

// CreateGroup adds a group to the site's list and places it on nodes; its
// own file gets its settings and first owner (OwnerID, if set) from the
// InitGroup this sends to the new group's log.
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
	return nil, a.Site(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM groups WHERE slug = ?`, c.Slug).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSlugTaken
		}
		if _, err := tx.Exec(`INSERT INTO groups (id, slug, name, visibility, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.GroupID, c.Slug, c.Name, vis, c.At); err != nil {
			return err
		}
		// The group's settings live here, with the site's. InitGroup gives
		// the group's own file the same first values (its copy). A private
		// group's FAQ starts private too.
		if _, err := tx.Exec(`INSERT INTO group_settings (group_id, name, description, visibility, public_faq) VALUES (?, ?, ?, ?, ?)`,
			c.GroupID, c.Name, c.Description, vis, vis == "public"); err != nil {
			return err
		}
		if err := placeNewGroup(tx, c.GroupID, c.At); err != nil {
			return err
		}
		return send(tx, &InitGroup{GroupID: c.GroupID, Name: c.Name, Description: c.Description,
			Visibility: vis, OwnerID: c.OwnerID, At: c.At}, c.At)
	})
}

func (*CreateGroup) siteLog() {}

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
