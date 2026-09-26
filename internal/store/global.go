package store

import (
	"strconv"
	"strings"
)

// Global is the site-wide configuration, from site.db's settings table.
// It replaced most of grus.conf: anything that should be the same on
// every node lives here, is changed on the admin page, and reaches every
// node through the site log. (grus.conf still has what a node needs
// before it can read site.db: its id, its files, the cluster.)
//
// Each field notes its key. A key with no row takes the default shown.
type Global struct {
	Operators []string // "operators": emails, one per line; these accounts get the operator flag

	// The SMTP relay for any domain without its own (domains table).
	// "smtp_host" empty means email is printed to the log rather than
	// sent, which is what you want on a laptop.
	SMTPHost string // "smtp_host"
	SMTPPort int    // "smtp_port", default 587
	SMTPUser string // "smtp_user"
	SMTPPass string // "smtp_pass"

	ACMEEmail string // "acme_email": contact address given to Let's Encrypt

	// AI. Nodes with a local model (ai_url in their grus.conf) all run
	// this one, so any of them answers the same way.
	AIModel   string // "ai_model": the model name in Ollama
	AIContext int    // "ai_context": context window in tokens, default 16384
	AskLimit  int    // "ask_daily_limit": quick answers per person per day, default 20
	FAQHour   int    // "faq_hour": UTC hour the nightly FAQ batch is queued, default 8
	// "digest_hour": UTC hour the daily email digest goes out, default 12
	DigestHour int
}

// GlobalKeys lists every key, in the order the admin page shows them.
var GlobalKeys = []string{"operators", "smtp_host", "smtp_port", "smtp_user", "smtp_pass", "acme_email",
	"ai_model", "ai_context", "ask_daily_limit", "faq_hour", "digest_hour"}

// GlobalRaw returns the settings table as stored: key to value. Missing
// keys are absent (they take their defaults).
func (s *Store) GlobalRaw() (map[string]string, error) {
	rows, err := s.Site().Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Global reads the site-wide configuration, with defaults filled in.
// It's a handful of rows, read when needed rather than cached, so a change
// on the admin page takes effect everywhere at once.
func (s *Store) Global() (*Global, error) {
	raw, err := s.GlobalRaw()
	if err != nil {
		return nil, err
	}
	// Values were checked by cmd.SetGlobal on the way in, so a bad number
	// here can't happen; if it somehow did, the default is the safe answer.
	num := func(key string, def int) int {
		if n, err := strconv.Atoi(raw[key]); err == nil {
			return n
		}
		return def
	}
	g := &Global{
		SMTPHost:   raw["smtp_host"],
		SMTPPort:   num("smtp_port", 587),
		SMTPUser:   raw["smtp_user"],
		SMTPPass:   raw["smtp_pass"],
		ACMEEmail:  raw["acme_email"],
		AIModel:    raw["ai_model"],
		AIContext:  num("ai_context", 16384),
		AskLimit:   num("ask_daily_limit", 20),
		FAQHour:    num("faq_hour", 8),
		DigestHour: num("digest_hour", 12),
	}
	for _, line := range strings.Split(raw["operators"], "\n") {
		if e := strings.ToLower(strings.TrimSpace(line)); e != "" {
			g.Operators = append(g.Operators, e)
		}
	}
	return g, nil
}

// IsOperator reports whether email is listed as an operator.
func (g *Global) IsOperator(email string) bool {
	for _, o := range g.Operators {
		if o == strings.ToLower(email) {
			return true
		}
	}
	return false
}
