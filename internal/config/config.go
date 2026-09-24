// Package config reads grus.conf.
//
// The format is deliberately plain: one "key = value" per line, "#" starts a
// comment, and keys that take a list (peer, operator) are simply repeated.
// It's easier to read and edit on a server than JSON, and it keeps secrets
// like the SMTP password out of the process arguments where `ps` shows them.
//
// See deploy/grus.conf.example for every key with an explanation.
package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Peer is another node the leader keeps in the cluster as a non-voter
// (in M0, that's the Studio's full copy).
type Peer struct {
	ID   string // stable Raft id, e.g. "studio"
	Addr string // host:port of its cluster port, resolved on every connect
}

// Config is everything a node needs to start. Zero values are filled in by
// Load with the defaults noted beside each field.
type Config struct {
	NodeID  string // Raft id of this node, e.g. "n1" or "studio" (required)
	NodeNum int    // 0-1023, distinct per node; goes into every id (default 1)
	DataDir string // databases, raft log, blobs (default /var/lib/grus)

	// PrimaryDomain seeds the domains table the first time the site
	// database is created. After that the table is the truth, and the
	// primary is changed with the SetPrimaryDomain command (admin page).
	PrimaryDomain string

	// Web serving. A node with neither address set (the Studio in M0) runs
	// only the cluster side: it keeps a full live copy and serves nothing.
	HTTPAddr  string // ":80" in production (ACME + redirect to HTTPS)
	HTTPSAddr string // ":443" in production
	Dev       bool   // plain HTTP on HTTPAddr, no certificates, non-Secure cookies
	ACMEEmail string // contact address given to Let's Encrypt

	// Cluster.
	ClusterAddr string // listen address for node-to-node mTLS, e.g. ":7946"
	Advertise   string // host:port other nodes use to reach this one
	Bootstrap   bool   // first start of the first voter creates the cluster
	Peers       []Peer // non-voters the leader keeps in the cluster
	TLSCA       string // cluster CA certificate (made by `grus ca init`)
	TLSCert     string // this node's certificate (made by `grus ca issue`)
	TLSKey      string // this node's key

	// Mail. With no SMTPHost, login emails are written to the log instead
	// of sent, which is what you want on a laptop.
	SMTPHost string
	SMTPPort int // default 587
	SMTPUser string
	SMTPPass string
	MailFrom string // default login@<primary domain>

	// Operators: emails whose accounts get the site operator flag when they
	// sign in.
	Operators []string

	// AI (plan section 9). A node with ai_url runs a model: it works the
	// background job queue and answers searches (the Studio). Web nodes list
	// those nodes as `worker` lines (their cluster addresses) to send
	// searches to them; a node that has a model and serves pages uses its
	// own too.
	AIURL     string   // Ollama, e.g. http://127.0.0.1:11434
	AIModel   string   // the model name in Ollama (pick one with `grus bench-llm`)
	AIContext int      // context window in tokens (default 16384)
	Workers   []string // cluster addresses of nodes with a model
	AskLimit  int      // search questions per person per day (default 20)
	// The hour (UTC, 0-23) the leader queues the nightly FAQ batch, when
	// the model is otherwise idle. Default 8: 3-4am in US Eastern time.
	FAQHour int
}

// Load reads and checks a config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c := &Config{NodeNum: 1, DataDir: "/var/lib/grus", SMTPPort: 587, FAQHour: 8}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		if err := c.set(strings.TrimSpace(key), strings.TrimSpace(val)); err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, c.check()
}

func (c *Config) set(key, val string) error {
	var err error
	switch key {
	case "node_id":
		c.NodeID = val
	case "node_num":
		c.NodeNum, err = strconv.Atoi(val)
	case "data_dir":
		c.DataDir = val
	case "primary_domain":
		c.PrimaryDomain = strings.ToLower(val)
	case "http_addr":
		c.HTTPAddr = val
	case "https_addr":
		c.HTTPSAddr = val
	case "dev":
		c.Dev, err = strconv.ParseBool(val)
	case "acme_email":
		c.ACMEEmail = val
	case "cluster_addr":
		c.ClusterAddr = val
	case "advertise":
		c.Advertise = val
	case "bootstrap":
		c.Bootstrap, err = strconv.ParseBool(val)
	case "peer":
		// peer = studio studio.example.com:7946
		id, addr, ok := strings.Cut(val, " ")
		if !ok {
			return fmt.Errorf("peer: expected \"<id> <host:port>\"")
		}
		c.Peers = append(c.Peers, Peer{ID: id, Addr: strings.TrimSpace(addr)})
	case "tls_ca":
		c.TLSCA = val
	case "tls_cert":
		c.TLSCert = val
	case "tls_key":
		c.TLSKey = val
	case "smtp_host":
		c.SMTPHost = val
	case "smtp_port":
		c.SMTPPort, err = strconv.Atoi(val)
	case "smtp_user":
		c.SMTPUser = val
	case "smtp_pass":
		c.SMTPPass = val
	case "mail_from":
		c.MailFrom = val
	case "operator":
		c.Operators = append(c.Operators, strings.ToLower(val))
	case "ai_url":
		c.AIURL = val
	case "ai_model":
		c.AIModel = val
	case "ai_context":
		c.AIContext, err = strconv.Atoi(val)
	case "worker":
		c.Workers = append(c.Workers, val)
	case "ask_daily_limit":
		c.AskLimit, err = strconv.Atoi(val)
	case "faq_hour":
		c.FAQHour, err = strconv.Atoi(val)
		if err == nil && (c.FAQHour < 0 || c.FAQHour > 23) {
			err = fmt.Errorf("faq_hour must be 0 to 23")
		}
	default:
		// Fail loudly: a misspelled key silently ignored is a bad afternoon.
		return fmt.Errorf("unknown key %q", key)
	}
	return err
}

// ToolNodeNum is the id node number that operator tools use.
const ToolNodeNum = 1023

func (c *Config) check() error {
	if c.NodeID == "" {
		return fmt.Errorf("node_id is required")
	}
	// 1023 is kept for offline tools like import-archive, which make ids
	// while the nodes are running and must never collide with them.
	if c.NodeNum < 0 || c.NodeNum > ToolNodeNum-1 {
		return fmt.Errorf("node_num must be 0-%d", ToolNodeNum-1)
	}
	if c.PrimaryDomain == "" {
		return fmt.Errorf("primary_domain is required")
	}
	if c.ClusterAddr == "" || c.Advertise == "" {
		return fmt.Errorf("cluster_addr and advertise are required")
	}
	if _, _, err := net.SplitHostPort(c.Advertise); err != nil {
		return fmt.Errorf("advertise: %v", err)
	}
	if c.TLSCA == "" || c.TLSCert == "" || c.TLSKey == "" {
		return fmt.Errorf("tls_ca, tls_cert and tls_key are required (see `grus ca`)")
	}
	if c.AIURL != "" && c.AIModel == "" {
		return fmt.Errorf("ai_url is set but ai_model isn't")
	}
	if c.MailFrom == "" {
		c.MailFrom = "login@" + c.PrimaryDomain
	}
	return nil
}

// IsOperator reports whether email is listed as an operator.
func (c *Config) IsOperator(email string) bool {
	for _, o := range c.Operators {
		if o == strings.ToLower(email) {
			return true
		}
	}
	return false
}
