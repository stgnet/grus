package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/store"
)

// The admin page's DNS and mail checks: for each domain, whether what the
// world sees matches what the site needs (docs/dns.md). They're here, not
// in make install, because DNS changes on its own schedule: the page shows
// the state now, and what each record should be when it isn't right.

// dnsCheck is one thing checked.
type dnsCheck struct {
	What   string // "nfb.group A", "SPF"
	OK     bool
	Found  string // what DNS has now
	Wanted string // what it should have, when that's known
}

// domainChecks are one domain's checks.
type domainChecks struct {
	Domain string
	Checks []dnsCheck
}

// lookupTimeout bounds each lookup, so a slow resolver can't hold the
// admin page up.
const lookupTimeout = 3 * time.Second

// checkDomains runs every domain's checks at once.
func (s *Server) checkDomains(domains []store.Domain, nodes []store.Node) []domainChecks {
	// The addresses of the nodes that serve pages (voter): a domain should
	// point at some of them. Not the others, such as the Studio, which is
	// reachable on the cluster port but serves no pages.
	nodeIPs := map[string]string{} // ip -> node id
	for _, nd := range nodes {
		if !nd.Voter {
			continue
		}
		host, _, err := net.SplitHostPort(nd.Addr)
		if err != nil {
			continue
		}
		for _, ip := range s.lookup(host) {
			nodeIPs[ip] = nd.ID
		}
	}
	out := make([]domainChecks, len(domains))
	var wg sync.WaitGroup
	for i, d := range domains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = domainChecks{Domain: d.Name, Checks: s.checkDomain(d.Name, nodeIPs)}
		}()
	}
	wg.Wait()
	return out
}

func (s *Server) checkDomain(domain string, nodeIPs map[string]string) []dnsCheck {
	var checks []dnsCheck
	var nodeList []string
	for ip, id := range nodeIPs {
		nodeList = append(nodeList, ip+" ("+id+")")
	}
	sort.Strings(nodeList)
	wantAddr := "the address of a node that serves pages: " + strings.Join(nodeList, ", ")

	// The bare name, www, and a made-up group name, which only the
	// wildcard record answers.
	for _, host := range []string{domain, "www." + domain, randomLabel() + "." + domain} {
		what := host
		if !strings.HasSuffix(host, "."+domain) || strings.HasPrefix(host, "www.") {
			what = host + " address"
		} else {
			what = "*." + domain + " address (any group)"
		}
		ips := s.lookup(host)
		c := dnsCheck{What: what, Found: strings.Join(ips, ", "), Wanted: wantAddr}
		for _, ip := range ips {
			if id, ok := nodeIPs[ip]; ok {
				c.OK = true
				c.Found += " (" + id + ")"
				break
			}
		}
		if len(ips) == 0 {
			c.Found = "nothing"
		}
		checks = append(checks, c)
	}

	// Mail: SPF and DMARC on the domain, and DKIM for each key this site
	// has set up for it (deploy/install-mail.sh records them).
	txts := s.lookupTXT(domain)
	spf := findPrefix(txts, "v=spf1")
	checks = append(checks, dnsCheck{What: "SPF (TXT on " + domain + ")", OK: spf != "", Found: orNothing(spf),
		Wanted: "v=spf1 ip4:<the address of each node that sends mail> -all"})
	dmarc := findPrefix(s.lookupTXT("_dmarc."+domain), "v=DMARC1")
	checks = append(checks, dnsCheck{What: "DMARC (TXT on _dmarc." + domain + ")", OK: dmarc != "", Found: orNothing(dmarc),
		Wanted: "v=DMARC1; p=none (tighten to quarantine, then reject, once reports look clean)"})
	for _, k := range s.dkimKeys(domain) {
		name := k.selector + "._domainkey." + domain
		found := strings.Join(s.lookupTXT(name), "")
		checks = append(checks, dnsCheck{What: "DKIM (TXT on " + name + ")",
			OK:    strings.Contains(strings.ReplaceAll(found, " ", ""), strings.ReplaceAll(k.value, " ", "")),
			Found: orNothing(found), Wanted: k.value})
	}
	return checks
}

type dkimKey struct{ selector, value string }

// dkimKeys lists the DKIM keys set up for a domain on this node: the files
// deploy/install-mail.sh leaves in <data_dir>/dkim/<domain>, each named
// for its selector and holding the TXT record's value.
func (s *Server) dkimKeys(domain string) []dkimKey {
	dir := filepath.Join(s.Store.Dir(), "dkim", domain)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []dkimKey
	for _, e := range ents {
		sel, ok := strings.CutSuffix(e.Name(), ".txt")
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, dkimKey{selector: sel, value: strings.TrimSpace(string(data))})
	}
	return out
}

// resolver is the DNS resolver for the checks: Server.Resolver, or the
// system's.
func (s *Server) resolver() *net.Resolver {
	if s.Resolver != nil {
		return s.Resolver
	}
	return net.DefaultResolver
}

func (s *Server) lookup(host string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ips, err := s.resolver().LookupHost(ctx, host)
	if err != nil {
		return nil
	}
	slices.Sort(ips)
	return ips
}

func (s *Server) lookupTXT(name string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	txts, _ := s.resolver().LookupTXT(ctx, name)
	return txts
}

func findPrefix(list []string, prefix string) string {
	for _, s := range list {
		if strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix)) {
			return s
		}
	}
	return ""
}

func orNothing(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// randomLabel is a group name nobody would pick, to test the wildcard.
func randomLabel() string {
	b := make([]byte, 5)
	rand.Read(b)
	return "dns-check-" + hex.EncodeToString(b)
}
