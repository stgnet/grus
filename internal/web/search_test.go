package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/ai"
	"github.com/stgnet/grus/internal/cmd"
)

// pickLLM answers search calls: phrases for the expand step, and a card for
// every thread for the pick step (including one it shouldn't be able to
// cite, which the page must drop).
type pickLLM struct{ slow time.Duration }

func (p pickLLM) Call(ctx context.Context, req ai.Request, out any) (ai.Usage, error) {
	if p.slow > 0 {
		select {
		case <-time.After(p.slow):
		case <-ctx.Done():
			return ai.Usage{}, ctx.Err()
		}
	}
	answer := `{}`
	switch {
	case strings.Contains(req.System, "search phrases"):
		answer = `{"phrases": ["fan"]}`
	case strings.Contains(req.System, "numbered THREADS"):
		var cards []string
		for i := 1; i <= strings.Count(req.Prompt, "\n\n"); i++ {
			cards = append(cards, fmt.Sprintf(`{"n": %d, "statement": "Thread %d reports a fan fix."}`, i, i))
		}
		answer = `{"cards": [` + strings.Join(cards, ",") + `]}`
	}
	return ai.Usage{}, json.Unmarshal([]byte(answer), out)
}

func TestSearchAndAsk(t *testing.T) {
	s := newSite(t)
	alice := s.signedIn("alice@example.com", "alice")
	G := "https://travato.nfb.group"
	for _, title := range []string{"Fridge fan rattle", "Roof fan won't close"} {
		expect(t, alice.upload(G+"/submit", map[string]string{"title": title, "body": "The fan is loud."}, nil), 303, "")
	}
	// A deleted post must not show up anywhere in search.
	w := alice.upload(G+"/submit", map[string]string{"title": "Fan gone", "body": "fan"}, nil)
	expect(t, alice.do("POST", G+w.Header().Get("Location")+"/delete", url.Values{}), 303, "/")

	// Plain results for anyone.
	body := s.browser().do("GET", G+"/search?q=fan", nil).Body.String()
	if !strings.Contains(body, "Fridge fan rattle") || !strings.Contains(body, "Roof fan") || strings.Contains(body, "Fan gone") {
		t.Fatalf("plain results:\n%s", body)
	}
	if strings.Contains(body, `id="cards"`) {
		t.Fatal("signed-out readers shouldn't get quick answers")
	}

	// No model anywhere: signed in, the page still doesn't ask.
	if strings.Contains(alice.do("GET", G+"/search?q=fan", nil).Body.String(), `id="cards"`) {
		t.Fatal("asked for cards with no AI configured")
	}
	w = alice.do("POST", G+"/ask", url.Values{"q": {"fan"}})
	if !strings.Contains(w.Body.String(), `class="soft"`) {
		t.Fatalf("no soft fail: %s", w.Body.String())
	}

	// With a model: cards, each checked on the way out.
	s.srv.AI = &ai.Pool{Local: &ai.Engine{LLM: pickLLM{}, Store: s.st, Meter: &ai.Meter{}}}
	s.srv.Meter = &ai.Meter{}
	if !strings.Contains(alice.do("GET", G+"/search?q=fan", nil).Body.String(), `id="cards"`) {
		t.Fatal("signed-in search didn't ask for cards")
	}
	w = alice.do("POST", G+"/ask", url.Values{"q": {"has anyone fixed a noisy fan?"}})
	cards := w.Body.String()
	if strings.Count(cards, "reports a fan fix") != 2 || !strings.Contains(cards, `data-generated="auto"`) {
		t.Fatalf("cards:\n%s", cards)
	}

	// Too slow: the soft fail, not an error.
	defer func(d time.Duration) { askTimeout = d }(askTimeout)
	askTimeout = 200 * time.Millisecond
	s.srv.AI = &ai.Pool{Local: &ai.Engine{LLM: pickLLM{slow: 10 * time.Second}, Store: s.st, Meter: &ai.Meter{}}}
	start := time.Now()
	w = alice.do("POST", G+"/ask", url.Values{"q": {"fan"}})
	if !strings.Contains(w.Body.String(), `class="soft"`) || time.Since(start) > askTimeout+2*time.Second {
		t.Fatalf("slow model: %s after %s", w.Body.String(), time.Since(start))
	}

	// The daily limit soft-fails too.
	s.srv.AI = &ai.Pool{Local: &ai.Engine{LLM: pickLLM{}, Store: s.st, Meter: &ai.Meter{}}}
	must(t, s.log, &cmd.SetGlobal{Values: map[string]string{"ask_daily_limit": "1"}})
	s.srv.asks = askCounter{}
	alice.do("POST", G+"/ask", url.Values{"q": {"fan"}})
	if !strings.Contains(alice.do("POST", G+"/ask", url.Values{"q": {"fan"}}).Body.String(), `class="soft"`) {
		t.Fatal("daily limit not applied")
	}

	// "Not what I was looking for" is the one time a question is stored.
	alice.do("POST", G+"/ask/feedback", url.Values{"q": {"noisy fan"}, "cited": {"1,2"}})
	db, _ := s.st.Group(42)
	var saved int
	db.QueryRow(`SELECT COUNT(*) FROM ai_feedback WHERE question = 'noisy fan'`).Scan(&saved)
	if saved != 1 {
		t.Fatal("feedback not saved")
	}

	// "Post this question" pre-fills the title and ticks the closest threads.
	form := alice.do("GET", G+"/submit?title=noisy+fan&related="+idOf(firstPost(t, s)), nil).Body.String()
	if !strings.Contains(form, `value="noisy fan"`) || !strings.Contains(form, `name="link"`) {
		t.Fatalf("prefill:\n%s", form)
	}
}

func firstPost(t *testing.T, s *testSite) string {
	db, _ := s.st.Group(42)
	var id int64
	db.QueryRow(`SELECT id FROM posts WHERE title = 'Fridge fan rattle'`).Scan(&id)
	return fmt.Sprintf("/p/%d", id)
}

func TestLinkAndMove(t *testing.T) {
	s := newSite(t)
	alice := s.signedIn("alice@example.com", "alice")
	bob := s.signedIn("bob@example.com", "bob")
	G := "https://travato.nfb.group"
	a := alice.upload(G+"/submit", map[string]string{"title": "Solar install, part 1", "body": "Panels."}, nil).Header().Get("Location")
	b := alice.upload(G+"/submit", map[string]string{"title": "Solar install, part 2", "body": "Wiring."}, nil).Header().Get("Location")
	// "Link to this" while writing links the new post at once.
	c := bob.upload(G+"/submit", map[string]string{"title": "Solar question", "link": idOf(a)}, nil).Header().Get("Location")

	page := alice.do("GET", G+a, nil).Body.String()
	if !strings.Contains(page, "Newer information") || !strings.Contains(page, "Solar question") {
		t.Fatalf("link note on the older post:\n%s", page)
	}
	if !strings.Contains(bob.do("GET", G+c, nil).Body.String(), "Earlier discussion") {
		t.Fatal("no link note on the newer post")
	}

	// Only the author (or a mod) links or moves a post.
	expect(t, bob.do("POST", G+a+"/link", url.Values{"other": {b}}), 404, "")
	expect(t, alice.do("POST", G+b+"/move", url.Values{"under": {idOf(c)}}), 400, "") // not her post
	expect(t, alice.do("POST", G+b+"/move", url.Values{"under": {idOf(a)}}), 303, "")

	// Part 2 is now an Update inside part 1; its own address goes there.
	w := s.browser().do("GET", G+b, nil)
	expect(t, w, 303, a+"#u"+idOf(b))
	page = s.browser().do("GET", G+a, nil).Body.String()
	if !strings.Contains(page, "Update</span> Solar install, part 2") {
		t.Fatalf("update section:\n%s", page)
	}
	// It leaves the feed, and moving it back out undoes that.
	if strings.Contains(s.browser().do("GET", G+"/", nil).Body.String(), "part 2") {
		t.Fatal("an update still in the feed")
	}
	expect(t, alice.do("POST", G+b+"/moveout", url.Values{}), 303, b)
	expect(t, s.browser().do("GET", G+b, nil), 200, "")
}

func TestSettingsOwnersOnly(t *testing.T) {
	s := newSite(t)
	alice := s.signedIn("alice@example.com", "alice")
	G := "https://travato.nfb.group"
	expect(t, alice.do("GET", G+"/settings", nil), 404, "")
	var id int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = 'alice'`).Scan(&id)
	must(t, s.log, &cmd.JoinGroup{GroupID: 42, UserID: id, At: 1})
	db, _ := s.st.Group(42)
	db.Exec(`UPDATE memberships SET role = 'owner' WHERE user_id = ?`, id)
	expect(t, alice.do("POST", G+"/settings", url.Values{"name": {"Travato"}, "description": {"Vans"}, "rules": {"Be kind"}}), 303, "/settings?saved=1")
	st, _ := s.st.GroupSettings(42)
	if st.AIEnabled || st.Rules != "Be kind" {
		t.Fatalf("settings: %+v", st)
	}
	if !strings.Contains(alice.do("GET", G+"/how-it-works", nil).Body.String(), "No post, comment, or search is sent to an outside AI service") {
		t.Fatal("how it works page")
	}
}
