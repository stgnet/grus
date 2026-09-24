package web

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stgnet/grus/internal/cmd"
)

// upload posts a multipart form, like a browser submitting photos.
func (b *browser) upload(rawURL string, fields map[string]string, files map[string][][]byte) *httptest.ResponseRecorder {
	b.site.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for field, list := range files {
		for i, data := range list {
			fw, _ := mw.CreateFormFile(field, fmt.Sprintf("p%d.jpg", i))
			fw.Write(data)
		}
	}
	mw.Close()
	r := httptest.NewRequest("POST", rawURL, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.site.h.ServeHTTP(w, r)
	return w
}

func testJPEG(t *testing.T) []byte {
	m := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for x := 0; x < 64; x++ {
		m.Set(x, 10, color.RGBA{200, 10, 10, 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// signedIn signs a new account in and gives it a handle.
func (s *testSite) signedIn(email, handle string) *browser {
	s.t.Helper()
	b := s.browser()
	b.do("POST", "https://nfb.group/login", url.Values{"email": {email}})
	token, _ := s.mailed()
	b.do("POST", "https://nfb.group/link/"+token, url.Values{})
	b.do("POST", "https://nfb.group/welcome", url.Values{"handle": {handle}})
	// Test accounts are a month old, so the new-account limits (links,
	// posts per hour) only apply where a test asks for them (newcomer).
	if _, err := s.st.Site().Exec(`UPDATE users SET created_at = created_at - 30*86400 WHERE email = ?`, email); err != nil {
		s.t.Fatal(err)
	}
	return b
}

// newcomer signs in an account made just now, which the new-account
// limits apply to.
func (s *testSite) newcomer(email, handle string) *browser {
	s.t.Helper()
	b := s.browser()
	b.do("POST", "https://nfb.group/login", url.Values{"email": {email}})
	token, _ := s.mailed()
	b.do("POST", "https://nfb.group/link/"+token, url.Values{})
	b.do("POST", "https://nfb.group/welcome", url.Values{"handle": {handle}})
	return b
}

var idRE = regexp.MustCompile(`/p/(\d+)`)

func TestPostCommentEditDelete(t *testing.T) {
	s := newSite(t)
	alice := s.signedIn("alice@example.com", "alice")
	bob := s.signedIn("bob@example.com", "bob")
	G := "https://travato.nfb.group"

	// Posting in an open group joins you. (A first post can't carry a
	// link, by the new-account limits, so alice says hello first.)
	expect(t, alice.upload(G+"/submit", map[string]string{"title": "Hello from a new owner"}, nil), 303, "")
	// The photo is stored and served.
	w := alice.upload(G+"/submit", map[string]string{"title": "Fridge fan rattle", "body": "Mine rattles at 40 mph. https://example.com/fan"},
		map[string][][]byte{"photos": {testJPEG(t)}})
	expect(t, w, 303, "")
	loc := w.Header().Get("Location")
	postURL := G + loc

	w = alice.do("GET", postURL, nil)
	body := w.Body.String()
	if !strings.Contains(body, "Fridge fan rattle") || !strings.Contains(body, `<a href="https://example.com/fan"`) {
		t.Fatalf("post page:\n%s", body)
	}
	hash := regexp.MustCompile(`/img/([0-9a-f]{64})/t`).FindStringSubmatch(body)
	if hash == nil {
		t.Fatal("no photo on the post page")
	}
	expect(t, s.browser().do("GET", G+"/img/"+hash[1], nil), 200, "")
	expect(t, s.browser().do("GET", G+"/img/"+strings.Repeat("0", 64), nil), 404, "")

	// The feed lists it, for readers who aren't signed in.
	if !strings.Contains(s.browser().do("GET", G+"/", nil).Body.String(), "Fridge fan rattle") {
		t.Fatal("post not in the feed")
	}

	// Comments and replies; a reply to a reply attaches to the top comment.
	expect(t, bob.upload(postURL+"/comment", map[string]string{"body": "Try a Noctua fan."}, nil), 303, "")
	db, _ := s.st.Group(42)
	var top int64
	db.QueryRow(`SELECT id FROM comments WHERE body = 'Try a Noctua fan.'`).Scan(&top)
	expect(t, alice.upload(postURL+"/comment", map[string]string{"body": "Which size?", "parent": fmt.Sprint(top)}, nil), 303, "")
	var reply int64
	db.QueryRow(`SELECT id FROM comments WHERE body = 'Which size?'`).Scan(&reply)
	expect(t, bob.upload(postURL+"/comment", map[string]string{"body": "92mm.", "parent": fmt.Sprint(reply)}, nil), 303, "")
	var parent int64
	db.QueryRow(`SELECT parent_id FROM comments WHERE body = '92mm.'`).Scan(&parent)
	if parent != top {
		t.Fatalf("reply to a reply has parent %d, want the top comment %d", parent, top)
	}
	var count int
	db.QueryRow(`SELECT comment_count FROM posts WHERE id = ?`, idOf(loc)).Scan(&count)
	if count != 3 {
		t.Fatalf("comment_count %d, want 3", count)
	}

	// Only the author can edit; the old version is kept.
	expect(t, bob.do("POST", postURL+"/edit", url.Values{"title": {"hijack"}}), 404, "")
	expect(t, alice.do("POST", postURL+"/edit", url.Values{"title": {"Fridge fan rattle (fixed)"}, "body": {"Noctua fixed it."}}), 303, "")
	var revs int
	db.QueryRow(`SELECT COUNT(*) FROM revisions WHERE kind = 'post'`).Scan(&revs)
	if revs != 1 {
		t.Fatalf("%d revisions, want 1", revs)
	}

	// Search index follows edits.
	var hits int
	db.QueryRow(`SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'noctua'`).Scan(&hits)
	if hits != 2 { // the edited post and bob's comment
		t.Fatalf("%d search hits for noctua, want 2", hits)
	}

	// The author deletes: gone for readers and from search, restorable by mods.
	expect(t, alice.do("POST", postURL+"/delete", url.Values{}), 303, "/")
	expect(t, s.browser().do("GET", postURL, nil), 404, "")
	expect(t, s.browser().do("GET", G+"/img/"+hash[1], nil), 404, "")
	db.QueryRow(`SELECT COUNT(*) FROM search_fts WHERE rowid = ?`, idOf(loc)).Scan(&hits)
	if hits != 0 {
		t.Fatal("deleted post still in the search index")
	}
	var purge int64
	db.QueryRow(`SELECT purge_after - deleted_at FROM posts WHERE id = ?`, idOf(loc)).Scan(&purge)
	if purge != cmd.AuthorDeleteKeep {
		t.Fatalf("purge window %d, want %d", purge, cmd.AuthorDeleteKeep)
	}

	// A mod (make bob one) can still see it, and restore it.
	var bobID int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = 'bob'`).Scan(&bobID)
	db.Exec(`UPDATE memberships SET role = 'mod' WHERE user_id = ?`, bobID)
	expect(t, bob.do("GET", postURL, nil), 200, "")
	expect(t, bob.do("POST", postURL+"/restore", url.Values{}), 303, "")
	expect(t, s.browser().do("GET", postURL, nil), 200, "")
}

func idOf(loc string) string {
	m := idRE.FindStringSubmatch(loc)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestPostingNeedsSignIn(t *testing.T) {
	s := newSite(t)
	w := s.browser().do("GET", "https://travato.nfb.group/submit", nil)
	expect(t, w, 303, "")
	if !strings.HasPrefix(w.Header().Get("Location"), "https://nfb.group/login?next=") {
		t.Fatalf("redirected to %s", w.Header().Get("Location"))
	}
}

func TestPrivateGroupPhotos(t *testing.T) {
	s := newSite(t)
	alice := s.signedIn("alice@example.com", "alice")
	G := "https://travato.nfb.group"
	w := alice.upload(G+"/submit", map[string]string{"title": "Campsite"}, map[string][][]byte{"photos": {testJPEG(t)}})
	expect(t, w, 303, "")
	body := alice.do("GET", G+w.Header().Get("Location"), nil).Body.String()
	hash := regexp.MustCompile(`/img/([0-9a-f]{64})/t`).FindStringSubmatch(body)[1]

	db, _ := s.st.Group(42)
	db.Exec(`UPDATE settings SET visibility = 'private'`)
	expect(t, s.browser().do("GET", G+"/img/"+hash, nil), 404, "")
	w = alice.do("GET", G+"/img/"+hash, nil)
	expect(t, w, 200, "")
	if !strings.HasPrefix(w.Header().Get("Cache-Control"), "private") {
		t.Fatal("private group's photo is publicly cacheable")
	}
}

func TestArchiveImport(t *testing.T) {
	s := newSite(t)
	c := &cmd.ImportPost{GroupID: 42, PostID: 900, OriginRef: "kb-1", Title: "Solar install", Body: "Two panels.",
		CreatedAt: 100, LastActivity: 200,
		Comments: []cmd.ArchiveComment{
			{ID: 901, OriginRef: "kb-1-a", Body: "What controller?", CreatedAt: 150},
			{ID: 902, OriginRef: "kb-1-b", ParentRef: "kb-1-a", Body: "Victron.", CreatedAt: 200},
		}}
	must(t, s.log, c)
	// Importing again updates in place.
	c.Body = "Two 200W panels."
	c.PostID, c.Comments[0].ID, c.Comments[1].ID = 950, 951, 952
	must(t, s.log, c)
	db, _ := s.st.Group(42)
	var posts, comments int
	db.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&posts)
	db.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&comments)
	if posts != 1 || comments != 2 {
		t.Fatalf("after re-import: %d posts, %d comments", posts, comments)
	}
	body := s.browser().do("GET", "https://travato.nfb.group/p/900", nil).Body.String()
	if !strings.Contains(body, "Two 200W panels.") || !strings.Contains(body, cmd.ArchiveAuthorHandle) {
		t.Fatalf("archive post page:\n%s", body)
	}
}
