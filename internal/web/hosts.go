package web

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/stgnet/grus/internal/store"
)

// Which site a request is for comes from its Host header alone. The rules
// (plan section 8, "Domains"):
//
//	nfb.group                  the home site: sign-in, home page, admin
//	www.nfb.group              301 to nfb.group
//	<slug>.nfb.group           that group (or 301 to its custom domain)
//	<custom domain>            the group whose main_host it is
//	<alias host>               301 to its group's main address
//	<alternate>, x.<alternate> 301 to the same place on the primary
//	anything else              "no such group"
//
// Every rule reads site.db, so adding a group, an alias or a domain takes
// effect on every node the moment the command is applied, with no restart
// or config change.

type siteKind int

const (
	siteUnknown siteKind = iota
	siteHome
	siteGroup
	siteRedirect
)

type route struct {
	kind     siteKind
	primary  string       // the primary domain at the time of the request
	group    *store.Group // siteGroup
	redirect string       // siteRedirect: scheme://host[:port], no path
}

// resolve works out which site host (a Host header, maybe with a port) is.
func (s *Server) resolve(hostport string) (*route, error) {
	host := strings.TrimSuffix(strings.ToLower(hostport), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	primary, err := s.Store.PrimaryDomain()
	if err != nil {
		return nil, err
	}
	rt := &route{primary: primary}

	switch {
	case host == primary:
		rt.kind = siteHome
		return rt, nil
	case host == "www."+primary:
		return s.redirectTo(rt, primary), nil
	case strings.HasSuffix(host, "."+primary):
		slug := strings.TrimSuffix(host, "."+primary)
		if strings.Contains(slug, ".") {
			return rt, nil // a.b.nfb.group: not a group
		}
		g, err := s.Store.GroupBySlug(slug)
		if err != nil || g == nil {
			return rt, err
		}
		if g.MainHost != "" {
			// The group moved to its own domain; old links follow it.
			return s.redirectTo(rt, g.MainHost), nil
		}
		rt.kind, rt.group = siteGroup, g
		return rt, nil
	}

	if g, err := s.Store.GroupByMainHost(host); err != nil || g != nil {
		if g != nil {
			rt.kind, rt.group = siteGroup, g
		}
		return rt, err
	}

	gid, found, err := s.Store.HostAlias(host)
	if err != nil {
		return nil, err
	}
	if found {
		if gid == 0 {
			return s.redirectTo(rt, primary), nil
		}
		g, err := s.Store.GroupByID(gid)
		if err != nil || g == nil {
			return rt, err
		}
		return s.redirectTo(rt, groupHost(g, primary)), nil
	}

	// Alternate domains: the old primary after a change, a short domain, a
	// typo domain. The bare name goes to the home page, and <slug>.<alt>
	// goes to <slug>.<primary>, so every old link keeps working.
	domains, err := s.Store.Domains()
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if d.Role != "alternate" {
			continue
		}
		if host == d.Name || host == "www."+d.Name {
			return s.redirectTo(rt, primary), nil
		}
		if sub, ok := strings.CutSuffix(host, "."+d.Name); ok && !strings.Contains(sub, ".") {
			return s.redirectTo(rt, sub+"."+primary), nil
		}
	}
	return rt, nil
}

func (s *Server) redirectTo(rt *route, host string) *route {
	rt.kind = siteRedirect
	rt.redirect = s.scheme() + "://" + host + s.PortSuffix
	return rt
}

// Building our own links. Nothing stored in the database is ever an
// absolute URL of ours: posts, notes and notifications store ids, and links
// are built here at render time from the current domains. That's what makes
// changing the primary domain cheap.

func (s *Server) scheme() string {
	if s.Dev {
		return "http"
	}
	return "https"
}

// groupHost is a group's main address: its custom domain, or
// <slug>.<primary>.
func groupHost(g *store.Group, primary string) string {
	if g.MainHost != "" {
		return g.MainHost
	}
	return g.Slug + "." + primary
}

func (s *Server) primaryURL(primary, path string) string {
	return s.scheme() + "://" + primary + s.PortSuffix + path
}

func (s *Server) groupURL(g *store.Group, primary, path string) string {
	return s.scheme() + "://" + groupHost(g, primary) + s.PortSuffix + path
}

// currentURL is the full URL of this request, for "come back here after
// signing in".
func (s *Server) currentURL(r *http.Request) string {
	return s.scheme() + "://" + r.Host + r.URL.RequestURI()
}

// safeNext checks a "where to go after signing in" URL. It must be one of
// our own sites, or sign-in would be an open redirect: a trusted-looking
// nfb.group link that drops people on a phishing page. Anything else becomes
// the home page.
func (s *Server) safeNext(raw, primary string) string {
	home := s.primaryURL(primary, "/")
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "/\\") {
		return s.primaryURL(primary, raw)
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
