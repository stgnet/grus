package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/tools"
)

// upload posts a multipart form with one file, as a browser would.
func (b *browser) upload1(url string, fields map[string]string, field, name, body string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile(field, name)
	io.WriteString(fw, body)
	mw.Close()
	r := httptest.NewRequest("POST", url, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	b.send(r)
	w := httptest.NewRecorder()
	b.site.h.ServeHTTP(w, r)
	b.keep(r, w)
	return w
}

// waitTool waits for the admin page's tool to finish, and returns its page.
func waitTool(t *testing.T, op *browser) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		page := op.do("GET", "https://nfb.group/admin/tools", nil).Body.String()
		if !strings.Contains(page, "Running: this page refreshes itself") {
			return page
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the tool never finished")
	return ""
}

// TestTools: the admin page's tools, which replaced the command-line ones.
// Only operators get the page; an archive imports into a group; a model
// measurement runs through Bench; one tool at a time.
func TestTools(t *testing.T) {
	s := newSite(t)
	member := s.signedIn("someone@example.com", "someone")
	expect(t, member.do("GET", "https://nfb.group/admin/tools", nil), 404, "")
	op := s.signedIn("scott@example.com", "scott")

	archive := `{"threads": [{"ref": "fb-1", "created": "2021-06-03", "title": "Fridge on propane",
		"body": "Won't cool", "comments": [{"created": 1622764800, "body": "Clean the burner"}]}]}`
	w := op.upload1("https://nfb.group/admin/tools/import", map[string]string{"group": "travato"}, "archive", "kb.json", archive)
	expect(t, w, 303, "/admin/tools")
	if page := waitTool(t, op); !strings.Contains(page, "imported 1 threads, 0 failed") {
		t.Fatalf("import report:\n%s", page)
	}
	if !strings.Contains(s.browser().do("GET", "https://travato.nfb.group/", nil).Body.String(), "Fridge on propane") {
		t.Fatal("the imported thread isn't in the group")
	}

	var gotModel string
	s.srv.Bench = func(ctx context.Context, a *tools.Archive, o tools.BenchOptions, out io.Writer) error {
		gotModel = o.Model
		io.WriteString(out, "digest: 3 runs\n")
		return nil
	}
	w = op.upload1("https://nfb.group/admin/tools/bench", map[string]string{"model": "qwen3:8b", "n": "5"}, "archive", "kb.json", archive)
	expect(t, w, 303, "/admin/tools")
	if page := waitTool(t, op); !strings.Contains(page, "digest: 3 runs") || gotModel != "qwen3:8b" {
		t.Fatalf("bench report (model %q):\n%s", gotModel, page)
	}

	// A bad load test address is refused on the page.
	expect(t, op.do("POST", "https://nfb.group/admin/tools/loadtest", map[string][]string{"url": {"nope"}, "c": {"1"}, "d": {"1"}}), 400, "")
}
