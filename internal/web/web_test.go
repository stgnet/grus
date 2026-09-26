package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/store"
)

// These tests drive the real handlers against a real SQLite store in a temp
// directory, with the local (non-Raft) log. Requests are built by hand so
// each can carry whatever Host header the test needs.

type testSite struct {
	t     *testing.T
	srv   *Server
	h     http.Handler
	mail  *bytes.Buffer
	st    *store.Store
	log   cluster.Log
	group *store.Group
}

func newSite(t *testing.T) *testSite {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lg, err := cluster.NewLocal(st)
	if err != nil {
		t.Fatal(err)
	}
	must(t, lg, &cmd.SeedGlobal{Domains: []string{"nfb.group"}, Values: map[string]string{"operators": "scott@example.com"}, At: 1})
	must(t, lg, &cmd.CreateGroup{GroupID: 42, Slug: "travato", Name: "Travato Owners", Description: "Vans", At: 1})
	g, _ := st.GroupBySlug("travato")

	blobs, err := blob.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	srv, err := New(&Server{
		Blobs:   blobs,
		Store:   st,
		Log:     lg,
		IDs:     ids.New(1),
		MailLog: buf,
		// No DNS in tests: every lookup fails at once.
		Resolver: &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("no DNS in tests")
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testSite{t: t, srv: srv, h: srv.Handler(), mail: buf, st: st, log: lg, group: g}
}

func must(t *testing.T, lg cluster.Log, c cmd.Command) {
	t.Helper()
	if _, err := lg.Apply(c); err != nil {
		t.Fatal(err)
	}
}

// browser keeps cookies between requests, like a browser would (ignoring
// Domain scoping, which the tests check separately).
type browser struct {
	site    *testSite
	cookies map[string]*http.Cookie // for nfb.group and its subdomains
	// Cookies for other domains (by domain): a real browser keeps them
	// apart from nfb.group's, and so must this one, to test that each
	// domain has its own sign-in.
	other map[string]map[string]*http.Cookie
	// remote, when set, is the address requests come from ("127.0.0.1:x"
	// for this machine; the default is httptest's, somewhere else).
	remote string
}

func (s *testSite) browser() *browser {
	return &browser{site: s, cookies: map[string]*http.Cookie{}, other: map[string]map[string]*http.Cookie{}}
}

// jar is the set of cookies this browser sends to host.
func (b *browser) jar(host string) map[string]*http.Cookie {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "nfb.group" || strings.HasSuffix(host, ".nfb.group") {
		return b.cookies
	}
	// Other domains: a cookie set for example.org covers its groups too.
	for d := range b.other {
		if host == d || strings.HasSuffix(host, "."+d) {
			return b.other[d]
		}
	}
	b.other[host] = map[string]*http.Cookie{}
	return b.other[host]
}

// send adds this browser's cookies for r's host, and keep stores the ones
// a response sets.
func (b *browser) send(r *http.Request) {
	for _, c := range b.jar(r.Host) {
		r.AddCookie(c)
	}
}

func (b *browser) keep(r *http.Request, w *httptest.ResponseRecorder) {
	jar := b.jar(r.Host)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(jar, c.Name)
		} else {
			jar[c.Name] = c
		}
	}
}

func (b *browser) do(method, rawURL string, form url.Values) *httptest.ResponseRecorder {
	b.site.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, rawURL, body)
	if b.remote != "" {
		r.RemoteAddr = b.remote
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	b.send(r)
	w := httptest.NewRecorder()
	b.site.h.ServeHTTP(w, r)
	b.keep(r, w)
	return w
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, location string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d; body: %s", w.Code, status, w.Body.String())
	}
	if location != "" && w.Header().Get("Location") != location {
		t.Fatalf("Location %q, want %q", w.Header().Get("Location"), location)
	}
}

func TestRouting(t *testing.T) {
	s := newSite(t)
	b := s.browser()

	expect(t, b.do("GET", "https://nfb.group/", nil), 200, "")
	expect(t, b.do("GET", "https://www.nfb.group/x?y=1", nil), 301, "https://nfb.group/x?y=1")
	w := b.do("GET", "https://travato.nfb.group/", nil)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), "Travato Owners") {
		t.Fatal("group page doesn't show the group")
	}
	expect(t, b.do("GET", "https://TRAVATO.nfb.group./", nil), 200, "") // case and trailing dot
	expect(t, b.do("GET", "https://nope.nfb.group/", nil), 404, "")
	expect(t, b.do("GET", "https://a.travato.nfb.group/", nil), 404, "")
	expect(t, b.do("GET", "https://example.com/", nil), 404, "")

	// A second domain is the same site, answered in its own domain: no
	// redirects, and its links point at itself.
	must(t, s.log, &cmd.AddDomain{Domain: "grus.example", At: 3})
	expect(t, b.do("GET", "https://travato.nfb.group/", nil), 200, "")
	w = b.do("GET", "https://grus.example/", nil)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), "https://travato.grus.example/") || strings.Contains(w.Body.String(), "nfb.group") {
		t.Fatalf("home page on grus.example should link only within grus.example:\n%s", w.Body.String())
	}
	expect(t, b.do("GET", "https://travato.grus.example/", nil), 200, "")
	expect(t, b.do("GET", "https://www.grus.example/faq", nil), 301, "https://grus.example/faq")

	// A domain under another listed domain goes by its own rules.
	must(t, s.log, &cmd.AddDomain{Domain: "eu.nfb.group", At: 4})
	expect(t, b.do("GET", "https://eu.nfb.group/", nil), 200, "")
	expect(t, b.do("GET", "https://travato.eu.nfb.group/", nil), 200, "")

	// Taken off the list, a domain is no longer answered.
	must(t, s.log, &cmd.RemoveDomain{Domain: "grus.example"})
	expect(t, b.do("GET", "https://travato.grus.example/", nil), 404, "")
	must(t, s.log, &cmd.RemoveDomain{Domain: "eu.nfb.group"})
	if _, err := s.log.Apply(&cmd.RemoveDomain{Domain: "nfb.group"}); !cmd.IsInput(err) {
		t.Fatalf("removing the last domain: %v", err)
	}
}

// TestDomainsAreSeparate: signing in on one domain signs in on that
// domain (and its groups) only, the sign-in email comes in that domain's
// name and through its own relay settings, and the account remembers the
// domain for later email.
func TestDomainsAreSeparate(t *testing.T) {
	s := newSite(t)
	must(t, s.log, &cmd.AddDomain{Domain: "grus.example", At: 2})
	must(t, s.log, &cmd.SetDomainMail{Domain: "grus.example", MailFrom: "hello@grus.example"})

	b := s.browser()
	expect(t, b.do("POST", "https://grus.example/login", url.Values{"email": {"bob@example.com"}}), 303, "/code")
	body := s.mail.String()
	if !strings.Contains(body, "From: hello@grus.example") || !strings.Contains(body, "https://grus.example/link/") {
		t.Fatalf("sign-in email for grus.example:\n%s", body)
	}
	token := regexp.MustCompile(`https://grus\.example/link/([A-Za-z0-9_-]+)`).FindStringSubmatch(body)[1]
	w := b.do("POST", "https://grus.example/link/"+token, url.Values{})
	expect(t, w, 303, "")
	var domain string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			domain = c.Domain
		}
	}
	if domain != "grus.example" {
		t.Fatalf("session cookie for %q, want grus.example", domain)
	}
	if b.jar("nfb.group")[sessionCookie] != nil {
		t.Fatal("signing in on grus.example signed in on nfb.group")
	}
	u, _ := s.st.UserByName("bob@example.com")
	if u == nil || u.Domain != "grus.example" {
		t.Fatalf("account's domain: %+v", u)
	}
}

func TestReservedSlug(t *testing.T) {
	s := newSite(t)
	if _, err := s.log.Apply(&cmd.CreateGroup{GroupID: 43, Slug: "login", Name: "Fake login", At: 1}); err == nil {
		t.Fatal("reserved slug accepted")
	}
}

// mailed pulls the link token and code out of the last email sent.
func (s *testSite) mailed() (token, code string) {
	s.t.Helper()
	body := s.mail.String()
	links := regexp.MustCompile(`https://nfb\.group/link/([A-Za-z0-9_-]+)`).FindAllStringSubmatch(body, -1)
	codes := regexp.MustCompile(`\n {4}(\d{6})`).FindAllStringSubmatch(strings.ReplaceAll(body, "\r\n", "\n"), -1)
	if len(links) == 0 || len(codes) == 0 {
		s.t.Fatalf("no link or code in mail:\n%s", body)
	}
	return links[len(links)-1][1], codes[len(codes)-1][1]
}

// TestSignInFromGroupPage follows a newcomer from a group page, through the
// emailed link and picking a handle, back to the group page, signed in.
func TestSignInFromGroupPage(t *testing.T) {
	s := newSite(t)
	b := s.browser()

	// The group page's Sign in link carries the group page as next.
	w := b.do("GET", "https://travato.nfb.group/", nil)
	if !strings.Contains(w.Body.String(), `https://nfb.group/login?next=https%3A%2F%2Ftravato.nfb.group%2F`) {
		t.Fatalf("group page has no sign-in link back to itself:\n%s", w.Body.String())
	}

	w = b.do("POST", "https://nfb.group/login", url.Values{"email": {"New@Example.com"}, "next": {"https://travato.nfb.group/"}})
	expect(t, w, 303, "/code")
	token, _ := s.mailed()

	// Opening the link (as an email scanner would) doesn't use it up.
	expect(t, b.do("GET", "https://nfb.group/link/"+token, nil), 200, "")
	expect(t, b.do("GET", "https://nfb.group/link/"+token, nil), 200, "")

	// Continue signs in; a new account picks a handle first.
	w = b.do("POST", "https://nfb.group/link/"+token, url.Values{})
	expect(t, w, 303, "/welcome?next="+url.QueryEscape("https://travato.nfb.group/"))
	sess := b.cookies[sessionCookie]
	if sess == nil || sess.Domain != "nfb.group" || !sess.HttpOnly || !sess.Secure {
		t.Fatalf("session cookie: %+v", sess)
	}

	expect(t, b.do("GET", "https://nfb.group/welcome?next=https://travato.nfb.group/", nil), 200, "")
	w = b.do("POST", "https://nfb.group/welcome", url.Values{"handle": {"ab"}, "next": {"https://travato.nfb.group/"}})
	expect(t, w, 400, "") // too short
	w = b.do("POST", "https://nfb.group/welcome", url.Values{"handle": {"vanlife"}, "next": {"https://travato.nfb.group/"}})
	expect(t, w, 303, "https://travato.nfb.group/")

	// Back on the group page, signed in (the cookie covers every subdomain).
	w = b.do("GET", "https://travato.nfb.group/", nil)
	if !strings.Contains(w.Body.String(), "vanlife") {
		t.Fatal("not signed in on the group page")
	}

	// The link is single-use.
	expect(t, s.browser().do("POST", "https://nfb.group/link/"+token, url.Values{}), 410, "")

	// Signing out ends the session everywhere, not just this cookie.
	raw := b.cookies[sessionCookie].Value
	expect(t, b.do("POST", "https://travato.nfb.group/logout", url.Values{}), 303, "/")
	other := s.browser()
	other.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: raw}
	if strings.Contains(other.do("GET", "https://travato.nfb.group/", nil).Body.String(), "vanlife") {
		t.Fatal("session still works after sign-out")
	}
}

func TestSignInWithCode(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	expect(t, b.do("POST", "https://nfb.group/login", url.Values{"email": {"a@example.com"}}), 303, "/code")
	_, code := s.mailed()
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}

	for i := 1; i < cmd.MaxCodeTries; i++ {
		expect(t, b.do("POST", "https://nfb.group/code", url.Values{"code": {wrong}}), 400, "")
	}
	// The last allowed wrong guess burns the sign-in, even for the right code.
	expect(t, b.do("POST", "https://nfb.group/code", url.Values{"code": {wrong}}), 410, "")
	expect(t, b.do("POST", "https://nfb.group/code", url.Values{"code": {code}}), 410, "")

	// A fresh email works.
	expect(t, b.do("POST", "https://nfb.group/login", url.Values{"email": {"a@example.com"}}), 303, "/code")
	_, code = s.mailed()
	w := b.do("POST", "https://nfb.group/code", url.Values{"code": {code}})
	expect(t, w, 303, "/welcome?next="+url.QueryEscape("https://nfb.group/"))
}

func TestNoOpenRedirect(t *testing.T) {
	s := newSite(t)
	home := "https://nfb.group/"
	for _, next := range []string{
		"https://evil.example/",
		"//evil.example/",
		"/\\evil.example/",
		"javascript:alert(1)",
		"https://travato.nfb.group.evil.example/",
		"https://user@travato.nfb.group/",
		"http://travato.nfb.group/", // wrong scheme
		"https://nope.nfb.group/",   // not a group
		"http://localhost/",         // localhost only from localhost
	} {
		if got := s.srv.safeNext(next, site{domain: "nfb.group"}); got != home {
			t.Errorf("safeNext(%q) = %q, want the home page", next, got)
		}
	}
	for next, want := range map[string]string{
		"https://travato.nfb.group/p/1?x=2": "https://travato.nfb.group/p/1?x=2",
		"/welcome":                          "https://nfb.group/welcome",
	} {
		if got := s.srv.safeNext(next, site{domain: "nfb.group"}); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestCrossSitePostRefused(t *testing.T) {
	s := newSite(t)
	r := httptest.NewRequest("POST", "https://nfb.group/login", strings.NewReader("email=a%40example.com"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST got %d, want 403", w.Code)
	}
	if s.mail.Len() != 0 {
		t.Fatal("cross-site POST sent an email")
	}
}

func TestAdminIsOperatorOnly(t *testing.T) {
	s := newSite(t)
	signIn := func(email string) *browser {
		b := s.browser()
		b.do("POST", "https://nfb.group/login", url.Values{"email": {email}})
		token, _ := s.mailed()
		b.do("POST", "https://nfb.group/link/"+token, url.Values{})
		return b
	}

	member := signIn("someone@example.com")
	expect(t, member.do("GET", "https://nfb.group/admin", nil), 404, "")
	expect(t, member.do("GET", "https://nfb.group/admin/network", nil), 404, "")
	expect(t, member.do("POST", "https://nfb.group/admin/groups", url.Values{"slug": {"x1"}, "name": {"X"}}), 404, "")

	op := signIn("scott@example.com")
	expect(t, op.do("GET", "https://nfb.group/admin", nil), 200, "")
	expect(t, op.do("GET", "https://nfb.group/admin/network", nil), 200, "")
	expect(t, op.do("POST", "https://nfb.group/admin/groups",
		url.Values{"slug": {"promaster"}, "name": {"ProMaster Vans"}}), 303, "/admin")
	expect(t, op.do("GET", "https://promaster.nfb.group/", nil), 200, "")

	// The creator is the new group's owner.
	g, _ := s.st.GroupBySlug("promaster")
	u, _ := s.st.UserBySession(auth.Hash(op.cookies[sessionCookie].Value), 0)
	m, _ := s.st.Membership(g.ID, u.ID)
	if m == nil || m.Role != "owner" {
		t.Fatalf("creator's membership: %+v", m)
	}
	// Bad input shows an error, not a crash.
	expect(t, op.do("POST", "https://nfb.group/admin/groups", url.Values{"slug": {"www"}, "name": {"W"}}), 400, "")

	// The node map: register two nodes, place the group on the small one,
	// see it listed, take it off again; then remove a node.
	must(t, s.log, &cmd.RegisterNode{ID: "n1", Addr: "vps1:7946", Voter: true, At: 1})
	must(t, s.log, &cmd.RegisterNode{ID: "small", Addr: "vps2:7946", At: 1})
	expect(t, member.do("POST", "https://nfb.group/admin/place", url.Values{"group": {"promaster"}, "node": {"small"}}), 404, "")
	expect(t, op.do("POST", "https://nfb.group/admin/place", url.Values{"group": {"promaster"}, "node": {"small"}, "action": {"place"}}), 303, "/admin")
	page := op.do("GET", "https://nfb.group/admin", nil).Body.String()
	if !strings.Contains(page, "<td>small</td>") || !strings.Contains(page, "<td>promaster</td>") {
		t.Fatalf("node map on the admin page:\n%s", page)
	}
	expect(t, op.do("POST", "https://nfb.group/admin/place", url.Values{"group": {"promaster"}, "node": {"small"}, "action": {"remove"}}), 303, "/admin")
	if strings.Contains(op.do("GET", "https://nfb.group/admin", nil).Body.String(), "<td>promaster</td>") {
		t.Fatal("still placed after taking it off")
	}
	// A group's last voter can't be taken off.
	expect(t, op.do("POST", "https://nfb.group/admin/place", url.Values{"group": {"promaster"}, "node": {"n1"}, "action": {"remove"}}), 400, "")
	expect(t, op.do("POST", "https://nfb.group/admin/nodes/remove", url.Values{"node": {"small"}}), 303, "/admin")
	if strings.Contains(op.do("GET", "https://nfb.group/admin", nil).Body.String(), "<td>small</td>") {
		t.Fatal("removed node still listed")
	}
}

func TestPrivateAndHiddenGroups(t *testing.T) {
	s := newSite(t)
	b := s.browser()

	s.st.Site().Exec(`UPDATE group_settings SET visibility = 'private' WHERE group_id = 42`)
	w := b.do("GET", "https://travato.nfb.group/", nil)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), "This group is private") {
		t.Fatal("private group shows content to a signed-out reader")
	}

	s.st.Site().Exec(`UPDATE group_settings SET visibility = 'hidden' WHERE group_id = 42`)
	expect(t, b.do("GET", "https://travato.nfb.group/", nil), 404, "")
	if strings.Contains(b.do("GET", "https://nfb.group/", nil).Body.String(), "Travato") {
		t.Fatal("hidden group listed on the home page")
	}
}

// TestPassOn checks which requests a node that doesn't hold everything
// passes on: a group's pages when it doesn't hold that group, and the home
// page and notifications (which gather from every group) when it doesn't
// hold them all. Sign-in stays local: it only needs site.db.
func TestPassOn(t *testing.T) {
	s := newSite(t)
	var passed []int64
	s.srv.Holds = func(id int64) bool { return false }
	s.srv.HoldsAll = func() bool { return false }
	s.srv.PassOn = func(w http.ResponseWriter, r *http.Request, id int64) bool {
		passed = append(passed, id)
		w.WriteHeader(299)
		return true
	}
	b := s.browser()
	expect(t, b.do("GET", "https://travato.nfb.group/", nil), 299, "")
	expect(t, b.do("GET", "https://nfb.group/", nil), 299, "")
	if len(passed) != 2 || passed[0] != 42 || passed[1] != 0 {
		t.Fatalf("passed on: %v", passed)
	}
	if code := b.do("GET", "https://nfb.group/login", nil).Code; code != 200 {
		t.Fatalf("sign-in page: %d", code)
	}
	// When no node could take it, the page is served here.
	s.srv.PassOn = func(http.ResponseWriter, *http.Request, int64) bool { return false }
	if code := b.do("GET", "https://travato.nfb.group/", nil).Code; code != 200 {
		t.Fatalf("fallback: %d", code)
	}
}

// TestRenderCache checks the public-page cache: a signed-out visitor's
// second view of a page comes from memory, any write to the group (or to
// site.db) makes the next view fresh, and signed-in visitors and private
// groups never touch it.
func TestRenderCache(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	anon := s.browser()
	first := anon.do("GET", G+"/", nil).Body.String()
	if second := anon.do("GET", G+"/", nil).Body.String(); second != first || s.srv.cache.hits.Load() != 1 {
		t.Fatalf("second view not from the cache (hits %d)", s.srv.cache.hits.Load())
	}
	alice := s.signedIn("alice@example.com", "alice")
	alice.upload(G+"/submit", map[string]string{"title": "Fresh post"}, nil)
	if !strings.Contains(anon.do("GET", G+"/", nil).Body.String(), "Fresh post") {
		t.Fatal("a cached page outlived a write")
	}
	hits := s.srv.cache.hits.Load()
	if !strings.Contains(alice.do("GET", G+"/", nil).Body.String(), "Fresh post") || s.srv.cache.hits.Load() != hits {
		t.Fatal("a signed-in view came from the cache")
	}
}

// TestConcurrentLoad is a small load test of the web layer in CI: many
// visitors at once, reading (signed out, through the render cache, and
// signed in) while others post and comment. Nothing may fail, and under
// the race detector nothing may race.
func TestConcurrentLoad(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	var writers []*browser
	for i := 0; i < 4; i++ {
		writers = append(writers, s.signedIn(fmt.Sprintf("w%d@example.com", i), fmt.Sprintf("writer%d", i)))
	}
	first := writers[0].upload(G+"/submit", map[string]string{"title": "Load test thread"}, nil).Header().Get("Location")
	var wg sync.WaitGroup
	fail := make(chan string, 100)
	for i, w := range writers {
		wg.Add(1)
		go func(i int, w *browser) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if c := w.upload(G+"/submit", map[string]string{"title": fmt.Sprintf("Post %d-%d", i, j)}, nil).Code; c != 303 {
					fail <- fmt.Sprintf("post: %d", c)
				}
				if c := w.upload(G+first+"/comment", map[string]string{"body": fmt.Sprintf("Comment %d-%d", i, j)}, nil).Code; c != 303 {
					fail <- fmt.Sprintf("comment: %d", c)
				}
			}
		}(i, w)
	}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := s.browser()
			if i%3 == 0 {
				b = writers[i%len(writers)]
			}
			for j := 0; j < 20; j++ {
				for _, p := range []string{"/", first, "/faq", "/about"} {
					if c := b.do("GET", G+p, nil).Code; c != 200 {
						fail <- fmt.Sprintf("GET %s: %d", p, c)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for f := range fail {
		t.Error(f)
	}
	if !strings.Contains(s.browser().do("GET", G+first, nil).Body.String(), "Comment 3-4") {
		t.Fatal("a comment written under load is missing")
	}
}
