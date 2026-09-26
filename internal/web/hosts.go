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
	domain   string       // the listed domain this request came in on ("" if none)
	group    *store.Group // siteGroup
	redirect string       // siteRedirect: scheme://host[:port], no path
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
	// example.org): its own rules apply, not the shorter one's.
	rt := &route{}
	for _, d := range domains {
		if (host == d.Name || strings.HasSuffix(host, "."+d.Name)) && len(d.Name) > len(rt.domain) {
			rt.domain = d.Name
		}
	}
	if rt.domain == "" {
		return rt, nil
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
	rt.redirect = s.scheme() + "://" + host + s.PortSuffix
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

// siteURL is a page of the home site on domain.
func (s *Server) siteURL(domain, path string) string {
	return s.scheme() + "://" + domain + s.PortSuffix + path
}

// groupURL is a page of a group, on domain: <slug>.<domain>.
func (s *Server) groupURL(g *store.Group, domain, path string) string {
	return s.scheme() + "://" + g.Slug + "." + domain + s.PortSuffix + path
}

// currentURL is the full URL of this request, for "come back here after
// signing in".
func (s *Server) currentURL(r *http.Request) string {
	return s.scheme() + "://" + r.Host + r.URL.RequestURI()
}

// safeNext checks a "where to go after signing in" URL. It must be one of
// our own sites, or sign-in would be an open redirect: a trusted-looking
// link of ours that drops people on a phishing page. Anything else becomes
// the home page of domain (the request's).
func (s *Server) safeNext(raw, domain string) string {
	home := s.siteURL(domain, "/")
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "/\\") {
		return s.siteURL(domain, raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != s.scheme() || u.User != nil || u.Host == "" {
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
