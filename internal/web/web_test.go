package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/mail"
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
	must(t, lg, &cmd.SetPrimaryDomain{Domain: "nfb.group", At: 1})
	must(t, lg, &cmd.CreateGroup{GroupID: 42, Slug: "travato", Name: "Travato Owners", Description: "Vans", At: 1})
	g, _ := st.GroupBySlug("travato")

	buf := &bytes.Buffer{}
	srv, err := New(&Server{
		Store:      st,
		Log:        lg,
		IDs:        ids.New(1),
		Mail:       &mail.Mailer{From: "login@nfb.group", Dev: buf},
		IsOperator: func(e string) bool { return e == "scott@example.com" },
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
	cookies map[string]*http.Cookie
}

func (s *testSite) browser() *browser { return &browser{site: s, cookies: map[string]*http.Cookie{}} }

func (b *browser) do(method, rawURL string, form url.Values) *httptest.ResponseRecorder {
	b.site.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, rawURL, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.site.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
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

	// An alias host redirects to its group, keeping the path.
	must(t, s.log, &cmd.AddHostAlias{Host: "travatoowners.org", GroupID: 42, At: 2})
	expect(t, b.do("GET", "https://travatoowners.org/p/5", nil), 301, "https://travato.nfb.group/p/5")

	// Changing the primary: the old one becomes an alternate, and every old
	// link redirects to the same page on the new primary.
	must(t, s.log, &cmd.SetPrimaryDomain{Domain: "grus.example", At: 3})
	expect(t, b.do("GET", "https://travato.nfb.group/p/5", nil), 301, "https://travato.grus.example/p/5")
	expect(t, b.do("GET", "https://nfb.group/login", nil), 301, "https://grus.example/login")
	expect(t, b.do("GET", "https://travato.grus.example/", nil), 200, "")
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
	} {
		if got := s.srv.safeNext(next, "nfb.group"); got != home {
			t.Errorf("safeNext(%q) = %q, want the home page", next, got)
		}
	}
	for next, want := range map[string]string{
		"https://travato.nfb.group/p/1?x=2": "https://travato.nfb.group/p/1?x=2",
		"/welcome":                          "https://nfb.group/welcome",
	} {
		if got := s.srv.safeNext(next, "nfb.group"); got != want {
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
	expect(t, member.do("POST", "https://nfb.group/admin/groups", url.Values{"slug": {"x1"}, "name": {"X"}}), 404, "")

	op := signIn("scott@example.com")
	expect(t, op.do("GET", "https://nfb.group/admin", nil), 200, "")
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
}

func TestPrivateAndHiddenGroups(t *testing.T) {
	s := newSite(t)
	db, _ := s.st.Group(42)
	b := s.browser()

	db.Exec(`UPDATE settings SET visibility = 'private'`)
	w := b.do("GET", "https://travato.nfb.group/", nil)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), "This group is private") {
		t.Fatal("private group shows content to a signed-out reader")
	}

	db.Exec(`UPDATE settings SET visibility = 'hidden'`)
	expect(t, b.do("GET", "https://travato.nfb.group/", nil), 404, "")
	if strings.Contains(b.do("GET", "https://nfb.group/", nil).Body.String(), "Travato") {
		t.Fatal("hidden group listed on the home page")
	}
}
