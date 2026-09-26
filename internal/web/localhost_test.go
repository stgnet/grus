package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// TestLocalhost: localhost is the whole site, like a listed domain, over
// plain HTTP and only from this machine; its groups are at /g/<slug>, and
// everything built while answering stays on localhost with its port.
func TestLocalhost(t *testing.T) {
	s := newSite(t)

	// From anywhere but this machine, localhost isn't a site at all.
	far := s.browser()
	expect(t, far.do("GET", "http://localhost/", nil), 404, "")

	b := s.browser()
	b.remote = "127.0.0.1:50000"
	expect(t, b.do("GET", "http://localhost:8080/", nil), 200, "")
	expect(t, b.do("GET", "http://localhost:8080/g/travato", nil), 301, "http://localhost:8080/g/travato/")
	w := b.do("GET", "http://localhost:8080/g/travato/", nil)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), "Travato Owners") {
		t.Fatal("localhost/g/travato/ doesn't show the group")
	}
	ownLinksUnder(t, w.Body.String(), "/g/travato/")
	expect(t, b.do("GET", "http://localhost:8080/g/travato/static/style.css", nil), 200, "")
	expect(t, b.do("GET", "http://localhost:8080/g/nope/", nil), 404, "")
	// <slug>.localhost works too, where the browser resolves it.
	expect(t, b.do("GET", "http://travato.localhost:8080/", nil), 200, "")

	// Signing in: to a group's page and back, all on localhost.
	w = b.do("GET", "http://localhost:8080/g/travato/join", nil)
	expect(t, w, 303, "")
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "http://localhost:8080/") ||
		!strings.Contains(loc, url.QueryEscape("http://localhost:8080/g/travato/join")) {
		t.Fatalf("sign-in from a group on localhost goes to %q", loc)
	}
	b.do("POST", "http://localhost:8080/login", url.Values{"email": {"scott@example.com"}})
	mail := s.mail.String()
	token := regexp.MustCompile(`http://localhost:8080/link/([A-Za-z0-9_-]+)`).FindStringSubmatch(mail)
	if token == nil {
		t.Fatalf("no localhost sign-in link in:\n%s", mail)
	}
	if !strings.Contains(mail, "login@nfb.group") {
		t.Errorf("the sign-in email isn't from a listed domain:\n%s", mail)
	}
	w = b.do("POST", "http://localhost:8080/link/"+token[1], url.Values{})
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil || session.Domain != "" || session.Secure {
		t.Fatalf("localhost session cookie: %+v (want host-only, not Secure)", session)
	}
	expect(t, b.do("GET", "http://localhost:8080/admin", nil), 200, "")
}

// TestGroupByPath: <domain>/g/<slug> is the group on any listed domain,
// the same page as <slug>.<domain>, with its links kept in the /g/ form.
func TestGroupByPath(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	w := b.do("GET", "https://nfb.group/g/travato/", nil)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), "Travato Owners") {
		t.Fatal("nfb.group/g/travato/ doesn't show the group")
	}
	ownLinksUnder(t, w.Body.String(), "/g/travato/")
	// Sign-in comes back to the /g/ form.
	w = b.do("GET", "https://nfb.group/g/travato/join", nil)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape("https://nfb.group/g/travato/join")) {
		t.Fatalf("sign-in from nfb.group/g/travato goes to %q", loc)
	}
	// The home page is still the home page (the page cache keeps the two
	// apart), and the subdomain form still links the usual way.
	if body := b.do("GET", "https://nfb.group/", nil).Body.String(); strings.Contains(body, "Travato Owners</h1>") {
		t.Fatal("nfb.group/ shows the group page")
	}
	if body := b.do("GET", "https://travato.nfb.group/", nil).Body.String(); strings.Contains(body, `href="/g/`) {
		t.Fatal("travato.nfb.group/ has /g/ links")
	}
	// Not a group: the plain "no such group" page.
	expect(t, b.do("GET", "https://nfb.group/g/nope/", nil), 404, "")
}

// TestPrefixRedirects: a path-addressed group's redirects to its own paths
// keep the /g/<slug>; ones to elsewhere are left alone.
func TestPrefixRedirects(t *testing.T) {
	for loc, want := range map[string]string{
		"/p/12":                      "/g/travato/p/12",
		"/":                          "/g/travato/",
		"//evil.example/":            "//evil.example/",
		"https://nfb.group/login?x=": "https://nfb.group/login?x=",
	} {
		rec := httptest.NewRecorder()
		w := &prefixRedirects{ResponseWriter: rec, prefix: "/g/travato"}
		http.Redirect(w, httptest.NewRequest("GET", "/", nil), loc, http.StatusSeeOther)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("redirect to %q: Location %q, want %q", loc, got, want)
		}
	}
}

// ownLinksUnder checks every link in page to one of the site's own paths
// is under prefix.
func ownLinksUnder(t *testing.T, page, prefix string) {
	t.Helper()
	links := regexp.MustCompile(`(?:href|action|src)="(/[^/"][^"]*)"`).FindAllStringSubmatch(page, -1)
	if len(links) == 0 {
		t.Fatal("no links on the page")
	}
	for _, l := range links {
		if !strings.HasPrefix(l[1], prefix) {
			t.Errorf("link %q isn't under %s", l[1], prefix)
		}
	}
}
