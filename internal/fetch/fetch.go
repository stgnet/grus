// Package fetch reads outside web pages for their summaries (plan section 5,
// "Mechanics"). It is deliberately not a crawler:
//
//   - It reads only the URL it's given, on a site the caller allows, and
//     follows redirects only to allowed sites. It never follows links on its
//     own (a seed list's links are returned to the caller, which queues
//     each as its own job).
//   - It won't connect to private, loopback, link-local or other internal
//     addresses. That's checked on the address actually dialed, after DNS
//     and on every redirect, so neither a hostile link nor a DNS trick can
//     point it at the home network the Studio sits on.
//   - It honors robots.txt, and caps size and time.
//
// It returns the page's main text for the model to summarize. The caller
// keeps only the summary, the title, the date and a hash of the text.
package fetch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Why a page couldn't be read. Each is shown to mods as the reason.
var (
	ErrNotAllowed = errors.New("that site isn't on the group's allowed list")
	ErrRobots     = errors.New("the site's robots.txt asks not to be read")
	ErrGone       = errors.New("the page no longer exists")
	ErrNotPage    = errors.New("not a web page that can be read (a PDF or other file)")
	ErrPrivate    = errors.New("that address is on a private network")
	ErrTooBig     = errors.New("the page is too big")
)

// UserAgent names the reader honestly, so a site can block it by name.
const UserAgent = "grus-source-reader/1.0 (+reads only pages members link to)"

const (
	maxBytes     = 3 << 20 // 3 MB of HTML is a very large page
	maxText      = 30000   // characters of text handed on
	maxRedirects = 5
)

// Fetcher reads pages. The zero value isn't usable; call New.
type Fetcher struct {
	client *http.Client
	// AllowPrivate lets tests read from 127.0.0.1. Never set in the server.
	AllowPrivate bool

	mu     sync.Mutex
	robots map[string]robotsEntry // by scheme://host
}

type robotsEntry struct {
	rules   *robotsRules // nil = no restrictions
	blocked bool         // robots.txt couldn't be read: treat the site as closed
	at      time.Time
}

// New makes a Fetcher with its own restricted HTTP client.
func New() *Fetcher {
	f := &Fetcher{robots: map[string]robotsEntry{}}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: f.checkAddr}
	transport := &http.Transport{
		// No proxy from the environment: the address check must see the
		// real destination, not a proxy's.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	f.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return f
}

// checkAddr refuses connections to anything but public addresses. It runs
// on the resolved IP right before connecting.
func (f *Fetcher) checkAddr(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ErrPrivate
	}
	if !f.AllowPrivate && !Public(ip) {
		return ErrPrivate
	}
	return nil
}

// cgnat is shared address space (100.64.0.0/10): carrier NAT and
// tailnets, never a public web site.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Public says whether an address is on the public internet.
func Public(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		if ip4[0] == 0 || cgnat.Contains(ip4) {
			return false
		}
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified())
}

// Page is what was read.
type Page struct {
	URL       string // after redirects
	Title     string
	Text      string // the main text, whitespace tidied, capped
	Published int64  // Unix seconds, 0 if the page doesn't say
	Links     []string
	Hash      string // of Text, to spot a changed page on re-check
}

// Allowed says whether a site may be read.
type Allowed func(host string) bool

// Get reads one page. allowed is checked for the URL and every redirect.
func (f *Fetcher) Get(ctx context.Context, raw string, allowed Allowed) (*Page, error) {
	u, err := checkURL(raw, allowed)
	if err != nil {
		return nil, err
	}
	if err := f.robotsOK(ctx, u, allowed); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html, text/plain;q=0.8")
	resp, err := f.do(req, allowed)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, ErrGone
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the site answered %s", resp.Status)
	}
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ctype != "text/html" && ctype != "application/xhtml+xml" && ctype != "text/plain" {
		return nil, ErrNotPage
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, ErrTooBig
	}
	p := &Page{URL: resp.Request.URL.String()}
	if ctype == "text/plain" {
		p.Text = tidy(string(body))
	} else {
		extract(p, body, resp.Request.URL)
	}
	if r := []rune(p.Text); len(r) > maxText {
		p.Text = string(r[:maxText])
	}
	sum := sha256.Sum256([]byte(p.Text))
	p.Hash = hex.EncodeToString(sum[:])
	return p, nil
}

// do sends a request, following redirects only to allowed, public sites.
func (f *Fetcher) do(req *http.Request, allowed Allowed) (*http.Response, error) {
	client := *f.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if _, err := checkURL(r.URL.String(), allowed); err != nil {
			return err
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		// Unwrap to our own reasons where we can (url.Error wraps them).
		for _, e := range []error{ErrPrivate, ErrNotAllowed} {
			if errors.Is(err, e) {
				return nil, e
			}
		}
		return nil, err
	}
	return resp, nil
}

// checkURL accepts http(s) URLs on the standard ports, on allowed sites.
func checkURL(raw string, allowed Allowed) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("not a web link")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" && allowed != nil && !allowed("port:"+p) {
		return nil, ErrNotAllowed
	}
	if allowed != nil && !allowed(strings.ToLower(u.Hostname())) {
		return nil, ErrNotAllowed
	}
	u.Fragment = ""
	return u, nil
}

// robotsOK checks the site's robots.txt, fetched once an hour per site.
// A missing robots.txt (any 4xx) means no restrictions; one that can't be
// read at all (5xx, no answer) means the site is treated as closed for
// now, as the robots.txt standard says.
func (f *Fetcher) robotsOK(ctx context.Context, u *url.URL, allowed Allowed) error {
	key := u.Scheme + "://" + u.Host
	f.mu.Lock()
	e, ok := f.robots[key]
	f.mu.Unlock()
	if !ok || time.Since(e.at) > time.Hour {
		e = robotsEntry{at: time.Now()}
		req, _ := http.NewRequestWithContext(ctx, "GET", key+"/robots.txt", nil)
		req.Header.Set("User-Agent", UserAgent)
		resp, err := f.do(req, allowed)
		switch {
		case err != nil:
			if errors.Is(err, ErrPrivate) || errors.Is(err, ErrNotAllowed) {
				return err
			}
			e.blocked = true
		case resp.StatusCode >= 500:
			resp.Body.Close()
			e.blocked = true
		case resp.StatusCode == 200:
			e.rules = parseRobots(io.LimitReader(resp.Body, 512<<10))
			resp.Body.Close()
		default:
			resp.Body.Close() // 4xx: no robots.txt, no restrictions
		}
		f.mu.Lock()
		f.robots[key] = e
		f.mu.Unlock()
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if e.blocked || e.rules != nil && !e.rules.allows(path) {
		return ErrRobots
	}
	return nil
}

// robotsRules are the Allow and Disallow lines that apply to us: the group
// naming "grus" if there is one, otherwise the "*" group.
type robotsRules struct {
	allow, disallow []string
}

func parseRobots(r io.Reader) *robotsRules {
	type group struct {
		agents          []string
		allow, disallow []string
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(v))
			lastWasAgent = true
			continue
		case "allow":
			if cur != nil && v != "" {
				cur.allow = append(cur.allow, v)
			}
		case "disallow":
			if cur != nil && v != "" {
				cur.disallow = append(cur.disallow, v)
			}
		}
		lastWasAgent = false
	}
	var star, ours *group
	for _, g := range groups {
		for _, a := range g.agents {
			if a == "*" && star == nil {
				star = g
			}
			if strings.Contains(a, "grus") && ours == nil {
				ours = g
			}
		}
	}
	if ours == nil {
		ours = star
	}
	if ours == nil {
		return nil
	}
	return &robotsRules{allow: ours.allow, disallow: ours.disallow}
}

// allows applies the most specific (longest) matching rule; Allow wins a
// tie, and no match means allowed.
func (r *robotsRules) allows(path string) bool {
	best, allowed := -1, true
	for _, p := range r.disallow {
		if robotsMatch(p, path) && len(p) > best {
			best, allowed = len(p), false
		}
	}
	for _, p := range r.allow {
		if robotsMatch(p, path) && len(p) >= best {
			best, allowed = len(p), true
		}
	}
	return allowed
}

// robotsMatch matches a robots.txt path pattern: a prefix, with * for any
// run of characters and a trailing $ anchoring the end.
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	rest := path[len(parts[0]):]
	if len(parts) == 1 {
		return !anchored || rest == ""
	}
	for i, part := range parts[1:] {
		if anchored && i == len(parts)-2 {
			return strings.HasSuffix(rest, part) // the last piece must end the path
		}
		j := strings.Index(rest, part)
		if j < 0 {
			return false
		}
		rest = rest[j+len(part):]
	}
	return true
}
