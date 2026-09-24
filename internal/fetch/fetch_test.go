package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\nAllow: /private/ok\n"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>Fan fix</title><meta property="article:published_time" content="2024-03-05T10:00:00Z"></head>
		<body><nav>Home | Forums</nav><article><h1>Fridge fan</h1><p>Replace the   fan.</p><script>evil()</script>
		<a href="/other">other</a><a href="https://elsewhere.example/x">off</a></article><footer>(c)</footer></body></html>`))
	})
	mux.HandleFunc("/private/ok", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("fine")) })
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF"))
	})
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://blocked.example/", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := New()
	ctx := context.Background()
	allowAll := func(string) bool { return true }

	// The real server refuses loopback addresses.
	if _, err := f.Get(ctx, srv.URL+"/page", allowAll); !errors.Is(err, ErrPrivate) {
		t.Fatalf("loopback: %v", err)
	}
	f.AllowPrivate = true

	p, err := f.Get(ctx, srv.URL+"/page", allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Fan fix" || p.Published == 0 || p.Hash == "" {
		t.Fatalf("page: %+v", p)
	}
	if !strings.Contains(p.Text, "Replace the fan.") || strings.Contains(p.Text, "evil") || strings.Contains(p.Text, "Forums") {
		t.Fatalf("text: %q", p.Text)
	}
	if len(p.Links) != 1 || !strings.HasSuffix(p.Links[0], "/other") {
		t.Fatalf("links: %v", p.Links)
	}

	if _, err := f.Get(ctx, srv.URL+"/private/x", allowAll); !errors.Is(err, ErrRobots) {
		t.Fatalf("robots: %v", err)
	}
	if _, err := f.Get(ctx, srv.URL+"/private/ok", allowAll); err != nil {
		t.Fatalf("robots allow: %v", err)
	}
	if _, err := f.Get(ctx, srv.URL+"/gone", allowAll); !errors.Is(err, ErrGone) {
		t.Fatalf("gone: %v", err)
	}
	if _, err := f.Get(ctx, srv.URL+"/pdf", allowAll); !errors.Is(err, ErrNotPage) {
		t.Fatalf("pdf: %v", err)
	}
	onlyLocal := func(h string) bool { return h == "127.0.0.1" || strings.HasPrefix(h, "port:") }
	if _, err := f.Get(ctx, srv.URL+"/away", onlyLocal); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("redirect off the allowed list: %v", err)
	}
	if _, err := f.Get(ctx, "http://example.com/", onlyLocal); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("not allowed: %v", err)
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.10": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.100.1.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false,
		"fd00::1": false, "::ffff:10.0.0.1": false,
	} {
		if got := Public(net.ParseIP(addr)); got != want {
			t.Errorf("Public(%s) = %v", addr, got)
		}
	}
}

func TestRobotsMatch(t *testing.T) {
	r := parseRobots(strings.NewReader("User-agent: other\nDisallow: /\n\nUser-agent: *\nDisallow: /*.pdf$\nDisallow: /search\n"))
	for path, want := range map[string]bool{
		"/": true, "/a/b.pdf": false, "/a/b.pdf.html": true, "/x.pdf/y.pdf": false, "/search?q=1": false, "/searching": false,
	} {
		if got := r.allows(path); got != want {
			t.Errorf("allows(%s) = %v", path, got)
		}
	}
}
