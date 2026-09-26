// Package config reads grus.conf.
//
// grus.conf holds only what's particular to one node, and what it needs
// before it can read site.db: who it is, where its files are, where it
// listens, and how to reach the rest of the cluster. Everything else (the
// domains, mail, operators, AI settings, every group and its settings) is
// the global level, in site.db, the same on every node and changed on
// the admin page.
//
// A few keys here are seeds for that global level (see Seed): they're
// used once, when a cluster is first created (or first upgraded to the
// global level), and ignored after that.
//
// The format is deliberately plain: one "key = value" per line, "#" starts a
// comment, and keys that take a list (join, domain, operator) are simply
// repeated. It's easier to read and edit on a server than JSON.
//
// See deploy/grus.conf.example for every key with an explanation.
package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Config is everything a node needs to start. Zero values are filled in by
// Load with the defaults noted beside each field.
type Config struct {
	NodeID  string // this node's id, e.g. "n1" or "studio" (default: the host name, up to the first dot)
	NodeNum int    // 0-1022, distinct per node; goes into every id (default -1: a free one, chosen at first start)
	DataDir string // databases, photos, cluster certificates (default /var/lib/grus; /usr/local/var/grus on macOS)

	// Web serving. A node with neither address set (the Studio) runs only
	// the cluster side: it keeps a full live copy and serves nothing.
	HTTPAddr  string // ":80" in production (ACME + redirect to HTTPS)
	HTTPSAddr string // ":443" in production
	Dev       bool   // plain HTTP on HTTPAddr, no certificates, non-Secure cookies

	// Cluster.
	ClusterAddr string   // listen address for node-to-node mTLS (default ":7946")
	Advertise   string   // host:port other nodes use to reach this one (default <host name>:7946)
	Voter       bool     // new groups are placed on it (a VPS that serves pages)
	Full        bool     // holds every group (the Studio)
	Join        []string // other nodes' cluster addresses; none means this is the first node
	// The cluster certificates (default <data_dir>/cluster/ca.crt,
	// node.crt, node.key). The service makes the node's own, and on the
	// first node the CA, when they're missing (cluster.EnsureCerts).
	TLSCA   string
	TLSCert string
	TLSKey  string

	// AIURL is this machine's own model server (Ollama, e.g.
	// http://127.0.0.1:11434), if it has one. It's a fact about this
	// machine's hardware, like DataDir; which model runs on it is a
	// global setting. A node with one works the background job queue and
	// answers searches for every node.
	AIURL string

	Seed Seed

	// Obsolete lists keys that are still accepted, so an old config
	// loads, but no longer do anything. The server logs them.
	Obsolete []string
}

// Seed is the first configuration of the global level, for a new cluster
// or one upgraded from when these lived in every node's grus.conf. The
// site log's leader applies it once (cmd.SeedGlobal); after that site.db
// is the truth, and these lines can be deleted.
type Seed struct {
	Domains  []string          // domain (repeatable); primary_domain is the old spelling
	MailFrom string            // mail_from: the first domain's sender
	Values   map[string]string // global settings by key: smtp_host, operators, ...
}

// seedKeys are the grus.conf keys that seed a global setting of the same
// name (see store.Global).
var seedKeys = map[string]bool{"smtp_host": true, "smtp_port": true, "smtp_user": true, "smtp_pass": true,
	"acme_email": true, "ai_model": true, "ai_context": true, "ask_daily_limit": true, "faq_hour": true, "digest_hour": true}

// Load reads and checks a config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c := &Config{NodeNum: -1, Seed: Seed{Values: map[string]string{}}}
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
	case "http_addr":
		c.HTTPAddr = val
	case "https_addr":
		c.HTTPSAddr = val
	case "dev":
		c.Dev, err = strconv.ParseBool(val)
	case "cluster_addr":
		c.ClusterAddr = val
	case "advertise":
		c.Advertise = val
	case "voter":
		c.Voter, err = strconv.ParseBool(val)
	case "full":
		c.Full, err = strconv.ParseBool(val)
	case "join":
		// join = vps1.nfb.group:7946
		if _, _, err := net.SplitHostPort(val); err != nil {
			return fmt.Errorf("join: %v", err)
		}
		c.Join = append(c.Join, val)
	case "tls_ca":
		c.TLSCA = val
	case "tls_cert":
		c.TLSCert = val
	case "tls_key":
		c.TLSKey = val
	case "ai_url":
		c.AIURL = val

	// Seeds for the global level.
	case "domain", "primary_domain":
		c.Seed.Domains = append(c.Seed.Domains, strings.ToLower(val))
	case "mail_from":
		c.Seed.MailFrom = val
	case "operator":
		ops := c.Seed.Values["operators"]
		if ops != "" {
			ops += "\n"
		}
		c.Seed.Values["operators"] = ops + strings.ToLower(val)

	// worker lines listed the nodes with a model; the node map has them
	// now. bootstrap created a Raft cluster; a node with no join line is
	// the first node now.
	case "worker", "bootstrap":
		c.Obsolete = append(c.Obsolete, key)
	default:
		if seedKeys[key] {
			c.Seed.Values[key] = val
			return nil
		}
		// Fail loudly: a misspelled key silently ignored is a bad afternoon.
		return fmt.Errorf("unknown key %q", key)
	}
	return err
}

// ToolNodeNum is the id node number that operator tools use.
const ToolNodeNum = 1023

// defaults fills in what the config leaves out. Only what's particular to
// a machine has a default here; a node needs nothing in grus.conf at all
// if the defaults suit it, apart from a join line on every node but the
// first.
func (c *Config) defaults() error {
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	if c.NodeID == "" {
		c.NodeID, _, _ = strings.Cut(strings.ToLower(host), ".")
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/grus"
		if runtime.GOOS == "darwin" {
			c.DataDir = "/usr/local/var/grus"
		}
	}
	if c.ClusterAddr == "" {
		c.ClusterAddr = ":7946"
	}
	if c.Advertise == "" {
		_, port, _ := net.SplitHostPort(c.ClusterAddr)
		c.Advertise = net.JoinHostPort(strings.ToLower(host), port)
	}
	certs := filepath.Join(c.DataDir, "cluster")
	if c.TLSCA == "" {
		c.TLSCA = filepath.Join(certs, "ca.crt")
	}
	if c.TLSCert == "" {
		c.TLSCert = filepath.Join(certs, "node.crt")
	}
	if c.TLSKey == "" {
		c.TLSKey = filepath.Join(certs, "node.key")
	}
	return nil
}

func (c *Config) check() error {
	if err := c.defaults(); err != nil {
		return err
	}
	// 1023 is kept for ids made by nothing but a node (it was for the
	// offline tools, which are gone); -1 means "choose one".
	if c.NodeNum < -1 || c.NodeNum > ToolNodeNum-1 {
		return fmt.Errorf("node_num must be 0-%d", ToolNodeNum-1)
	}
	if _, _, err := net.SplitHostPort(c.Advertise); err != nil {
		return fmt.Errorf("advertise: %v", err)
	}
	if len(c.Join) == 0 {
		// The first node takes new groups; there's nowhere else for them.
		c.Voter = true
	}
	return nil
}
