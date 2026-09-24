package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
)

// Signing in on a group's own domain (see internal/cmd/bounce.go for the
// idea). The pieces, in the order a visitor meets them:
//
//  1. route: a GET on a group's own domain from someone not signed in
//     there is sent to the primary's /bounce, once (bouncedCookie keeps a
//     signed-out visitor from going round forever).
//  2. bounce, on the primary: signed in there? Make a one-time code for
//     that domain and send the visitor to its /_bounce. Not signed in?
//     Straight back where they came from.
//  3. bounceBack, on the group's domain: trade the code for a session
//     there, set its cookie, and carry on to the page they asked for.
//
// Sign-in from the group's domain goes to the primary's sign-in page, and
// comes back through step 2, so it ends up signed in on both.

// bouncedCookie marks a browser that's been bounced recently, so a
// signed-out visitor is bounced once, not on every page.
const bouncedCookie = "grus_bounced"

// ownDomain reports whether a group is served on its own domain, where the
// primary's session cookie doesn't reach.
func ownDomain(rt *route) bool {
	return rt.kind == siteGroup && rt.group.MainHost != ""
}

// needsBounce reports whether this request should go through the primary
// to pick up a session: a page view on a group's own domain, from a
// browser that has no session here and hasn't just been bounced.
func (s *Server) needsBounce(r *http.Request, rt *route) bool {
	if !ownDomain(rt) || r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/_bounce") ||
		strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/img/") {
		return false
	}
	if _, err := r.Cookie(bouncedCookie); err == nil {
		return false
	}
	return s.user(r) == nil
}

// startBounce sends the visitor to the primary's /bounce.
func (s *Server) startBounce(w http.ResponseWriter, r *http.Request, rt *route) {
	http.SetCookie(w, &http.Cookie{Name: bouncedCookie, Value: "1", Path: "/", MaxAge: 3600,
		HttpOnly: true, Secure: !s.Dev, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.primaryURL(rt.primary, "/bounce?to="+queryEscape(s.currentURL(r))), http.StatusSeeOther)
}

// bounce, on the primary, sends a signed-in visitor back to a group's own
// domain with a one-time code for a session there.
func (s *Server) bounce(w http.ResponseWriter, r *http.Request) {
	to, err := url.Parse(r.URL.Query().Get("to"))
	if err != nil || to.Host == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Only to a group's own domain: never anywhere else, or this would be
	// a way to send someone's session to a site of the attacker's choice.
	back, err := s.resolve(to.Host)
	if err != nil || !ownDomain(back) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	host := back.group.MainHost
	path := to.EscapedPath()
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		path = "/"
	}
	if to.RawQuery != "" {
		path += "?" + to.RawQuery
	}
	dest := s.scheme() + "://" + host + s.PortSuffix
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, dest+path, http.StatusSeeOther)
		return
	}
	raw, hash := auth.NewToken()
	now := s.Now().Unix()
	if _, err := s.Log.Apply(&cmd.StartBounce{CodeHash: hash, UserID: u.ID, Host: host, ExpiresAt: now + cmd.BounceTTL, At: now}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, dest+"/_bounce?code="+queryEscape(raw)+"&next="+queryEscape(path), http.StatusSeeOther)
}

// bounceBack, on a group's own domain, trades a bounce code for a session.
func (s *Server) bounceBack(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	raw, hash := auth.NewToken()
	expires := s.Now().Add(sessionTTL)
	_, err := s.Log.Apply(&cmd.FinishBounce{CodeHash: auth.Hash(r.URL.Query().Get("code")), Host: rt.group.MainHost,
		SessionHash: hash, SessionExpires: expires.Unix(), UserAgentHint: uaHint(r.UserAgent()), At: s.Now().Unix()})
	if err != nil && !errors.Is(err, cmd.ErrLoginDead) {
		s.serverError(w, r, err)
		return
	}
	if err == nil {
		// A cookie for this domain only (no Domain attribute).
		s.setSession(w, "", raw, expires)
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// sessionDomain is the Domain for the session cookie on this request's
// site: the primary (covering every <slug>.<primary>), or none (this host
// only) on a group's own domain.
func sessionDomain(rt *route) string {
	if ownDomain(rt) {
		return ""
	}
	return rt.primary
}

// adminGroupHost gives a group its own domain, or takes it away.
func (s *Server) adminGroupHost(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	slug := strings.ToLower(strings.TrimSpace(r.FormValue("group")))
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	form := map[string]string{"group": slug, "domain": host}
	g, err := s.Store.GroupBySlug(slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if g == nil {
		s.adminFail(w, r, u, errors.New("no group "+slug), form)
		return
	}
	if _, err := s.Log.Apply(&cmd.SetGroupHost{GroupID: g.ID, Host: host, At: s.Now().Unix()}); err != nil {
		s.adminFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
