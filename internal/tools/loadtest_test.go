package tools

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLoadTest runs the load tester against a stand-in site: it should
// find the posts the front page links to, read them, and report.
func TestLoadTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<a href="/p/1">one</a> <a href="/p/2">two</a> <a href="/p/1">again</a>`)
			return
		}
		fmt.Fprint(w, "page")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r, err := runLoad(ctx, srv.URL, 4, []string{"/search?q=x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.paths, " ") != "/ /faq /search?q=x /p/1 /p/2" {
		t.Fatalf("paths: %v", r.paths)
	}
	if r.statuses[200] == 0 || r.errors != 0 {
		t.Fatalf("statuses %v, errors %d", r.statuses, r.errors)
	}
	var out bytes.Buffer
	r.print(&out, 300*time.Millisecond)
	if !strings.Contains(out.String(), "5 distinct") || !strings.Contains(out.String(), "99% under") {
		t.Fatalf("report:\n%s", out.String())
	}
}
