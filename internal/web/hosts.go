package web

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/stgnet/grus/internal/store"
)

// Which site a request is for comes from its Host header alone, against
// the one global list of domains (site.db's domains table). Every listed
// domain is equal: each is the whole site, with the same groups and the
// same content, and a request is answered in the domain it came in on.
// So with example.org and example.net both listed:
//
//	example.org                the home site: sign-in, home page, admin
//	www.example.org            301 to example.org
//	<slug>.example.org         that group
//	example.net, ...           exactly the same, with example.net links
//	anything else              "no such group"
//
// That lets each domain's DNS point at different nodes (any node answers
// any domain), and a node be tried out on its own by the domain that
// leads to it.
//
// Two more ways in, for testing without DNS records:
//
//	<domain>/g/<slug>/...      the group, on any domain, exactly as at
//	                           <slug>.<domain>/... (a domain whose wildcard
//	                           record isn't set up yet still works)
//	localhost[:port]           a built-in domain, equal to the listed ones,
//	                           over plain HTTP; its groups are always
//	                           localhost/g/<slug>
//
// Links built while answering follow the way the request came in: a page
// reached by /g/<slug> links to /g/<slug>/... (see withBase), and every
// localhost page stays on localhost with its port, so browsing never
// leaves the address that works.
//
// localhost is only answered for requests that really come from this
// machine (see isLoopback): otherwise anyone could send "Host: localhost"
// to a public node's port 80 and use the site over plain HTTP. An ssh
// tunnel (ssh -L 8080:localhost:80 vps1) arrives from this machine, so a
// node can be tried from anywhere that way.
//
// Every rule reads site.db, so adding a group or a domain takes effect on
// every node the moment the command is applied, with no restart or config
// change.

type siteKind int

const (
	siteUnknown siteKind = iota
	siteHome
	siteGroup
	siteRedirect
)

type route struct {
	kind     siteKind
	domain   string       // the listed domain this request came in on ("" if none), or "localhost"
	group    *store.Group // siteGroup
	redirect string       // siteRedirect: scheme://host[:port], no path
	// at is how links built while answering this request address the site.
	at site
	// prefix is "/g/<slug>" when the group was addressed by path: its
	// pages' own links and redirects get it put back (withBase).
	prefix string
}

// site is an address of the whole site, for building links: a domain, and
// whether its groups are addressed by path (<domain>/g/<slug>) rather than
// as <slug>.<domain>.
type site struct {
	domain string // a listed domain, or "localhost" with the port it was reached on
	paths  bool
}

// localDomain is the built-in domain, answered only for requests from this
// machine.
const localDomain = "localhost"

// isLocal reports whether a domain (maybe with a port) is localhost.
func isLocal(domain string) bool {
	host := domain
	if h, _, err := net.SplitHostPort(domain); err == nil {
		host = h
	}
	return host == localDomain
}

// siteAt is the address to use for a domain with no request to follow:
// email, for one. Groups on localhost can only be reached by path.
func siteAt(domain string) site { return site{domain: domain, paths: isLocal(domain)} }

// isLoopback reports whether a request came from this machine.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveRequest works out which site a request is for, from its host and,
// for a group addressed as /g/<slug>, its path. It returns the request to
// hand on: for such a group, with /g/<slug> taken off the path, so the
// group's pages see the same paths either way.
func (s *Server) resolveRequest(r *http.Request) (*route, *http.Request, error) {
	rt, err := s.resolve(r.Host)
	if err != nil {
		return nil, r, err
	}
	if rt.domain == localDomain && !isLoopback(r) {
		return &route{}, r, nil // localhost from elsewhere: not a host we serve
	}
	if rt.kind != siteHome {
		return rt, r, nil
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/g/")
	if !ok {
		return rt, r, nil
	}
	slug, path, _ := strings.Cut(rest, "/")
	g, err := s.Store.GroupBySlug(strings.ToLower(slug))
	if err != nil {
		return nil, r, err
	}
	if g == nil {
		rt.kind = siteUnknown
		return rt, r, nil
	}
	rt.kind, rt.group = siteGroup, g
	rt.prefix = "/g/" + g.Slug
	rt.at.paths = true
	if !strings.Contains(rest, "/") {
		// /g/travato: the group's front page is /g/travato/, so relative
		// links on it work the same as at travato.<domain>/. (route adds
		// the request's path to the redirect.)
		rt.kind, rt.redirect = siteRedirect, s.origin(rt.at)
		r2 := r.Clone(r.Context())
		r2.URL.Path += "/"
		r2.URL.RawPath = ""
		return rt, r2, nil
	}
	// The same request, with the group's own path.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + path
	r2.URL.RawPath = ""
	return rt, r2, nil
}

// resolve works out which site host (a Host header, maybe with a port) is.
func (s *Server) resolve(hostport string) (*route, error) {
	host := strings.TrimSuffix(strings.ToLower(hostport), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	domains, err := s.Store.Domains()
	if err != nil {
		return nil, err
	}
	// The listed domain this host is, or is under. The longest wins, in
	// case one listed domain is under another (a.example.org and
	// example.org): its own rules apply, not the shorter one's. localhost
	// is always there, as if listed.
	names := []string{localDomain}
	for _, d := range domains {
		names = append(names, d.Name)
	}
	rt := &route{}
	for _, name := range names {
		if (host == name || strings.HasSuffix(host, "."+name)) && len(name) > len(rt.domain) {
			rt.domain = name
		}
	}
	if rt.domain == "" {
		return rt, nil
	}
	rt.at = site{domain: rt.domain}
	if rt.domain == localDomain {
		// Links keep the port the browser used (a tunnel's, say), and
		// groups are reached by path: <slug>.localhost doesn't resolve
		// everywhere.
		rt.at = site{domain: localDomain, paths: true}
		if _, port, err := net.SplitHostPort(strings.ToLower(hostport)); err == nil {
			rt.at.domain = net.JoinHostPort(localDomain, port)
		}
	}
	if host == rt.domain {
		rt.kind = siteHome
		return rt, nil
	}
	sub := strings.TrimSuffix(host, "."+rt.domain)
	if sub == "www" {
		return s.redirectTo(rt, rt.domain), nil
	}
	if strings.Contains(sub, ".") {
		return rt, nil // a.b.example.org: not a group
	}
	g, err := s.Store.GroupBySlug(sub)
	if err != nil || g == nil {
		return rt, err
	}
	rt.kind, rt.group = siteGroup, g
	return rt, nil
}

func (s *Server) redirectTo(rt *route, host string) *route {
	rt.kind = siteRedirect
	rt.redirect = s.origin(site{domain: host})
	return rt
}

// Building our own links. Nothing stored in the database is ever an
// absolute URL of ours: posts, notes and notifications store ids, and links
// are built here at render time, in the domain of the request being
// answered (or, for email, the domain the person signed in on). That's
// what keeps every domain equal.

func (s *Server) scheme() string {
	if s.Dev {
		return "http"
	}
	return "https"
}

// schemeFor is the scheme a host is served on: plain HTTP for localhost
// (Let's Encrypt can't certify it; see LocalOr), otherwise the site's.
func (s *Server) schemeFor(host string) string {
	if isLocal(host) {
		return "http"
	}
	return s.scheme()
}

// origin is scheme://host[:port] for a site address. localhost's domain
// carries its own port; the others take the configured one (dev only).
func (s *Server) origin(at site) string {
	if isLocal(at.domain) {
		return "http://" + at.domain
	}
	return s.scheme() + "://" + at.domain + s.PortSuffix
}

// siteURL is a page of the home site.
func (s *Server) siteURL(at site, path string) string {
	return s.origin(at) + path
}

// groupURL is a page of a group: <slug>.<domain>, or <domain>/g/<slug>
// where groups are addressed by path.
func (s *Server) groupURL(g *store.Group, at site, path string) string {
	if at.paths {
		return s.origin(at) + "/g/" + g.Slug + path
	}
	return s.origin(site{domain: g.Slug + "." + at.domain}) + path
}

// currentURL is the full URL of this request, for "come back here after
// signing in". A group reached by path gets its /g/<slug> back.
func (s *Server) currentURL(r *http.Request) string {
	if rt := routeOf(r); rt != nil && rt.domain != "" {
		host := r.Host
		if isLocal(host) {
			return "http://" + host + rt.prefix + r.URL.RequestURI()
		}
		return s.scheme() + "://" + host + rt.prefix + r.URL.RequestURI()
	}
	return s.schemeFor(r.Host) + "://" + r.Host + r.URL.RequestURI()
}

// safeNext checks a "where to go after signing in" URL. It must be one of
// our own sites, or sign-in would be an open redirect: a trusted-looking
// link of ours that drops people on a phishing page. Anything else becomes
// the home page of domain (the request's).
func (s *Server) safeNext(raw string, at site) string {
	home := s.siteURL(at, "/")
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "/\\") {
		return s.siteURL(at, raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != s.schemeFor(u.Host) || u.User != nil || u.Host == "" {
		return home
	}
	// localhost only from localhost: from a real domain it's never where
	// someone meant to go.
	if isLocal(strings.ToLower(u.Host)) != isLocal(at.domain) {
		return home
	}
	rt, err := s.resolve(u.Host)
	if err != nil || (rt.kind != siteHome && rt.kind != siteGroup) {
		return home
	}
	return u.String()
}

func queryEscape(s string) string { return url.QueryEscape(s) }

type routeKey struct{}

func withRoute(r *http.Request, rt *route) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
}

func routeOf(r *http.Request) *route {
	rt, _ := r.Context().Value(routeKey{}).(*route)
	return rt
}
