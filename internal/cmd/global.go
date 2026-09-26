package cmd

import (
	"database/sql"
	"net/mail"
	"sort"
	"strconv"
	"strings"
)

// The global settings (site.db's settings table; store.Global reads them
// and lists what each key means). They're changed on the admin page with
// SetGlobal, and reach every node through the site log.

// globalRules checks a global setting's value on its way in. An empty
// value is always allowed: it removes the key, which then takes its
// default.
var globalRules = map[string]func(string) error{
	"operators":       emailLines,
	"smtp_host":       noBreaks,
	"smtp_port":       intRange(1, 65535),
	"smtp_user":       noBreaks,
	"smtp_pass":       noBreaks,
	"acme_email":      oneEmail,
	"ai_model":        noBreaks,
	"ai_context":      intRange(2048, 1<<20),
	"ask_daily_limit": intRange(1, 1000),
	"faq_hour":        intRange(0, 23),
	"digest_hour":     intRange(0, 23),
}

func noBreaks(v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return Invalid("must be one line")
	}
	return nil
}

func intRange(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return Invalid("must be a number from %d to %d", lo, hi)
		}
		return nil
	}
}

func oneEmail(v string) error {
	if _, err := mail.ParseAddress(v); err != nil {
		return Invalid("must be an email address")
	}
	return nil
}

func emailLines(v string) error {
	for _, line := range strings.Split(v, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if err := oneEmail(line); err != nil {
				return Invalid("%q isn't an email address", line)
			}
		}
	}
	return nil
}

// SetGlobal changes global settings: only the keys given, each to its new
// value, or back to its default when the value is "".
type SetGlobal struct {
	Values map[string]string
}

func (c *SetGlobal) Apply(a *Applier) (any, error) {
	keys, err := checkGlobal(c.Values)
	if err != nil {
		return nil, err
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		for _, k := range keys {
			if err := putGlobal(tx, k, c.Values[k]); err != nil {
				return err
			}
		}
		return nil
	})
}

// checkGlobal checks every value and returns the keys sorted, so every
// node writes them in the same order.
func checkGlobal(values map[string]string) ([]string, error) {
	var keys []string
	for k, v := range values {
		check, ok := globalRules[k]
		if !ok {
			return nil, Invalid("%s isn't a global setting", k)
		}
		if v = strings.TrimSpace(v); v != "" {
			if err := check(v); err != nil {
				return nil, Invalid("%s %v", strings.ReplaceAll(k, "_", " "), err)
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func putGlobal(tx *sql.Tx, key, value string) error {
	if key != "smtp_pass" {
		value = strings.TrimSpace(value)
	}
	if value == "" {
		_, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, key)
		return err
	}
	_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SeedGlobal is the first configuration of a new site, or of a site
// upgraded from when these settings were in each node's grus.conf. The
// site log's leader submits it at every start, from its grus.conf, and
// it does something only once: the first time. After that site.db is the
// truth, and grus.conf's copies of these values are ignored.
type SeedGlobal struct {
	Values   map[string]string // global settings (keys as in SetGlobal)
	Domains  []string          // listed if not already
	MailFrom string            // the old mail_from: the first domain's sender, if it hasn't one
	At       int64
}

// seededKey marks that SeedGlobal has run. It's in the settings table, but
// isn't one of the settings: SetGlobal refuses it.
const seededKey = "seeded_at"

func (c *SeedGlobal) Apply(a *Applier) (any, error) {
	keys, err := checkGlobal(c.Values)
	if err != nil {
		return nil, err
	}
	for _, d := range c.Domains {
		if err := ValidDomain(d); err != nil {
			return nil, err
		}
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = ?`, seededKey).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		for _, k := range keys {
			if err := putGlobal(tx, k, c.Values[k]); err != nil {
				return err
			}
		}
		for _, d := range c.Domains {
			if _, err := tx.Exec(`INSERT INTO domains (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, d, c.At); err != nil {
				return err
			}
		}
		if c.MailFrom != "" && len(c.Domains) > 0 {
			if _, err := tx.Exec(`UPDATE domains SET mail_from = ? WHERE name = ? AND mail_from = ''`, c.MailFrom, c.Domains[0]); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, seededKey, strconv.FormatInt(c.At, 10))
		return err
	})
}
