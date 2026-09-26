package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stgnet/grus/internal/cmd"
)

// makeMod makes a signed-in account a mod of the test group.
func (s *testSite) makeMod(handle string) {
	s.t.Helper()
	var id int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = ?`, handle).Scan(&id)
	must(s.t, s.log, &cmd.JoinGroup{GroupID: 42, UserID: id, At: 1})
	db, _ := s.st.Group(42)
	db.Exec(`UPDATE memberships SET role = 'mod' WHERE user_id = ?`, id)
}

func TestFAQPages(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	mod := s.signedIn("mod@example.com", "moddy")
	s.makeMod("moddy")
	alice := s.signedIn("alice@example.com", "alice")

	// Only mods write entries.
	expect(t, alice.do("GET", G+"/faq/new", nil), 404, "")
	w := mod.do("POST", G+"/faq/new", url.Values{"question": {"Which tires fit?"}, "answer": {"Owners report 225/75R16 fits."},
		"topic": {"0"}, "new_topic": {"Tires"}})
	expect(t, w, 303, "")
	entry := w.Header().Get("Location")

	page := s.browser().do("GET", G+"/faq", nil).Body.String()
	if !strings.Contains(page, "Which tires fit?") || !strings.Contains(page, "Tires") {
		t.Fatalf("FAQ page:\n%s", page)
	}
	// The group's front page pins the FAQ and points newcomers at it.
	front := s.browser().do("GET", G+"/", nil).Body.String()
	if !strings.Contains(front, "Start with the FAQ") || !strings.Contains(front, "Group FAQ") {
		t.Fatalf("front page:\n%s", front)
	}

	// Members comment on an entry.
	expect(t, alice.do("POST", G+entry+"/comment", url.Values{"body": {"Out of date for 2024 models."}}), 303, entry+"#comments")
	if !strings.Contains(s.browser().do("GET", G+entry, nil).Body.String(), "Out of date for 2024") {
		t.Fatal("comment not shown")
	}

	// Edit, then roll back from the history.
	var topic int64
	db, _ := s.st.Group(42)
	db.QueryRow(`SELECT id FROM faq_topics WHERE title = 'Tires'`).Scan(&topic)
	expect(t, mod.do("POST", G+entry+"/edit", url.Values{"question": {"Which tires fit?"}, "answer": {"Owners report 245s rub."},
		"topic": {fmt.Sprint(topic)}}), 303, entry)
	hist := mod.do("GET", G+entry+"/history", nil).Body.String()
	if !strings.Contains(hist, "225/75R16") || !strings.Contains(hist, "245s rub") {
		t.Fatalf("history:\n%s", hist)
	}
	var first int64
	db.QueryRow(`SELECT id FROM faq_history WHERE answer LIKE '%225/75R16%'`).Scan(&first)
	expect(t, mod.do("POST", G+entry+"/rollback", url.Values{"version": {fmt.Sprint(first)}}), 303, entry)
	if !strings.Contains(s.browser().do("GET", G+entry, nil).Body.String(), "225/75R16") {
		t.Fatal("rollback")
	}
	expect(t, alice.do("GET", G+entry+"/history", nil), 404, "")

	// Browse by topic: a post tagged by its author shows there.
	loc := alice.upload(G+"/submit", map[string]string{"title": "Tire pressure on the highway", "body": "What do you run?"}, nil).Header().Get("Location")
	expect(t, alice.do("POST", G+loc+"/topics", url.Values{"topic": {fmt.Sprint(topic)}}), 303, loc)
	tp := s.browser().do("GET", G+fmt.Sprintf("/faq/t/%d", topic), nil).Body.String()
	if !strings.Contains(tp, "Tire pressure on the highway") || !strings.Contains(tp, "Which tires fit?") {
		t.Fatalf("topic page:\n%s", tp)
	}
	if !strings.Contains(s.browser().do("GET", G+loc, nil).Body.String(), `class="chip"`) {
		t.Fatal("topic chip on the post")
	}

	// The root FAQ on the bare domain: operators write it; it lists groups.
	op := s.signedIn("scott@example.com", "scott")
	expect(t, alice.do("GET", "https://nfb.group/faq/new", nil), 404, "")
	expect(t, op.do("POST", "https://nfb.group/faq/new", url.Values{"question": {"What is this site?"},
		"answer": {"A home for discussion groups."}, "new_topic": {"About"}}), 303, "")
	root := s.browser().do("GET", "https://nfb.group/faq", nil).Body.String()
	if !strings.Contains(root, "What is this site?") || !strings.Contains(root, "Travato Owners") || !strings.Contains(root, "Its FAQ covers: Tires") {
		t.Fatalf("root FAQ:\n%s", root)
	}
}

func TestSummaryFolding(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	alice := s.signedIn("alice@example.com", "alice")
	loc := alice.upload(G+"/submit", map[string]string{"title": "Fridge fan", "body": "Rattles"}, nil).Header().Get("Location")
	for i := 1; i <= 12; i++ {
		alice.do("POST", G+loc+"/comment", url.Values{"body": {fmt.Sprintf("comment number %d", i)}})
	}
	db, _ := s.st.Group(42)
	var post int64
	fmt.Sscan(idOf(loc), &post)
	var ids []int64
	rows, _ := db.Query(`SELECT id FROM comments WHERE post_id = ? ORDER BY id`, post)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var version int64
	db.QueryRow(`SELECT thread_version FROM posts WHERE id = ?`, post).Scan(&version)
	var job int64
	db.QueryRow(`SELECT id FROM jobs WHERE kind = 'summary' AND ref_id = ?`, post).Scan(&job)
	must(t, s.log, &cmd.ClaimJob{GroupID: 42, JobID: job, Worker: "w", At: 1 << 40})
	must(t, s.log, &cmd.SetSummary{GroupID: 42, JobID: job, Worker: "w", PostID: post, Version: version, NoteID: 7,
		Text: "Most report a loose fan.", Covers: ids[:9], Useful: []int64{ids[4]},
		Tangents: []cmd.Tangent{{From: ids[1], To: ids[3], About: "tire pressure"}}, At: 2})

	page := s.browser().do("GET", G+loc, nil).Body.String()
	for _, want := range []string{"Summary of", "Most report a loose fan.", "Show the", "replies about tire pressure", "Show everything in order"} {
		if !strings.Contains(page, want) {
			t.Fatalf("arranged page lacks %q:\n%s", want, page)
		}
	}
	// The answer comes before the folded ones.
	if strings.Index(page, "comment number 5") > strings.Index(page, "comment number 1<") {
		t.Fatal("useful comment not moved up")
	}
	plain := s.browser().do("GET", G+loc+"?order=time", nil).Body.String()
	if strings.Contains(plain, "Show the") || strings.Contains(plain, "replies about") {
		t.Fatal("?order=time still folds")
	}
}

func TestOutsideSourcePages(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	mod := s.signedIn("mod@example.com", "moddy")
	s.makeMod("moddy")
	alice := s.signedIn("alice@example.com", "alice")
	loc := alice.upload(G+"/submit", map[string]string{"title": "Fridge fan", "body": "Rattles"}, nil).Header().Get("Location")

	// A described link shows on the post at once.
	expect(t, alice.do("POST", G+"/sources/new", url.Values{"post": {idOf(loc)}, "url": {"https://www.facebook.com/groups/1/posts/2"},
		"description": {"A member reports the 92mm fan swap fixed it."}}), 303, loc+"#elsewhere")
	page := s.browser().do("GET", G+loc, nil).Body.String()
	if !strings.Contains(page, "92mm fan swap") || !strings.Contains(page, "Open on facebook.com") {
		t.Fatalf("post with a source:\n%s", page)
	}
	// Facebook without a description is refused, with a message.
	expect(t, alice.do("POST", G+"/sources/new", url.Values{"url": {"https://facebook.com/x"}}), 400, "")

	// A member's link from a site not allowed waits in the mods' queue.
	alice.do("POST", G+"/sources/new", url.Values{"url": {"https://forum.example.org/t/123"}})
	expect(t, alice.do("GET", G+"/mod/sources", nil), 404, "")
	q := mod.do("GET", G+"/mod/sources", nil).Body.String()
	if !strings.Contains(q, "forum.example.org/t/123") {
		t.Fatalf("mod queue:\n%s", q)
	}
	expect(t, mod.do("POST", G+"/mod/sources/allow", url.Values{"domain": {"example.org"}}), 303, "/mod/sources")
	if !strings.Contains(mod.do("GET", G+"/mod/sources", nil).Body.String(), "example.org <form") {
		t.Fatal("allowed site not listed")
	}

	// Anyone can have a page taken down, at once.
	var src int64
	db, _ := s.st.Group(42)
	db.QueryRow(`SELECT id FROM sources WHERE via = 'described'`).Scan(&src)
	w := s.browser().do("POST", G+fmt.Sprintf("/sources/%d/remove", src), url.Values{"reason": {"I wrote that"}})
	if !strings.Contains(w.Body.String(), "Removed") {
		t.Fatalf("removal: %s", w.Body.String())
	}
	if strings.Contains(s.browser().do("GET", G+loc, nil).Body.String(), "92mm fan swap") {
		t.Fatal("removed source still shown")
	}
}
