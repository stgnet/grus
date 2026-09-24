package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/fetch"
)

// A long thread gets a summary note covering all but its latest comments,
// and the nudges the model found, checked against the thread.
func TestSummaryAndNudges(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Fridge fan rattle", Body: "Rattles.", At: at})
	for i := int64(1); i <= 12; i++ {
		e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 100 + i, PostID: 100, UserID: 5, Body: fmt.Sprintf("comment %d", i), At: at + i})
	}
	e.now = e.now.Add(time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE kind = 'summary' AND text LIKE 'Most report%'`); n != 1 {
		t.Fatal("no summary note")
	}
	if n := e.count(`SELECT COUNT(*) FROM note_sources WHERE covers = 1`); n != 9 {
		t.Fatalf("summary covers %d comments, want 9 (the latest 3 stay open)", n)
	}
	// "useful": [5, 99] keeps comment 5 only; the tangent is 2..4; comment
	// 1 is superseded by 6.
	if n := e.count(`SELECT COUNT(*) FROM nudges WHERE kind = 'rank' AND target_id = 105`); n != 1 {
		t.Fatal("rank nudge")
	}
	if n := e.count(`SELECT COUNT(*) FROM nudges WHERE kind = 'tangent' AND target_id = 102 AND value = 104 AND reason = 'tire pressure'`); n != 1 {
		t.Fatal("tangent nudge")
	}
	if n := e.count(`SELECT COUNT(*) FROM nudges WHERE kind = 'superseded' AND target_id = 101 AND value = 106`); n != 1 {
		t.Fatal("superseded nudge")
	}
	// A mod reverses the rank nudge; the next run doesn't make it again.
	var id int64
	db, _ := e.st.Group(1)
	db.QueryRow(`SELECT id FROM nudges WHERE kind = 'rank'`).Scan(&id)
	e.apply(&cmd.ReverseNudge{GroupID: 1, NudgeID: id, By: 5, At: e.now.Unix()})
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 200, PostID: 100, UserID: 5, Body: "one more", At: e.now.Unix()})
	e.now = e.now.Add(2 * time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM nudges WHERE kind = 'rank'`); n != 1 {
		t.Fatalf("%d rank nudges after a mod reversed it, want just the reversed one", n)
	}
	// A mod removing the summary keeps it removed.
	db.QueryRow(`SELECT id FROM notes WHERE kind = 'summary'`).Scan(&id)
	e.apply(&cmd.RemoveNote{GroupID: 1, NoteID: id, By: 5, At: e.now.Unix()})
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 201, PostID: 100, UserID: 5, Body: "and another", At: e.now.Unix()})
	e.now = e.now.Add(2 * time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE kind = 'summary' AND state = 'active'`); n != 0 {
		t.Fatal("a removed summary came back")
	}
}

// Linked threads become a FAQ entry at the nightly batch; changes to its
// threads make it stale and the next batch rewrites it; a locked entry
// gets a suggestion instead; a repeat question joins the entry.
func TestFAQBatch(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Fridge fan rattle", Body: "Rattles at 40 mph.", At: at})
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 200, UserID: 5, Title: "Noisy fridge fan", Body: "Fan rattle on the highway.", At: at + 60})
	for i := int64(1); i <= 5; i++ {
		e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 100 + i, PostID: 100, UserID: 5, Body: "same here", At: at + 100 + i})
	}
	e.now = e.now.Add(time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM post_links WHERE state = 'active'`); n != 1 {
		t.Fatal("posts not linked")
	}
	// An outside page on the same thing, so the entry can cite it.
	e.apply(&cmd.AddSource{GroupID: 1, SourceID: 900, URL: "https://www.example.com/bulletin", Summary: "The bulletin says the fridge fan was revised in 2021.", Via: cmd.ViaMember, AddedBy: 5, At: at})

	e.apply(&cmd.QueueFAQ{At: e.now.Unix()})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM faq_entries WHERE question LIKE 'How do owners%'`); n != 1 {
		t.Fatal("no entry from the cluster")
	}
	if n := e.count(`SELECT COUNT(*) FROM faq_sources`); n != 2 {
		t.Fatalf("%d sources, want both linked threads", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM faq_topics WHERE title = 'Fridge'`); n != 1 {
		t.Fatal("new topic not made")
	}
	if n := e.count(`SELECT COUNT(*) FROM source_links WHERE faq_entry_id != 0`); n != 1 {
		t.Fatal("outside page not cited")
	}
	// A second batch with nothing new makes no second entry.
	e.apply(&cmd.QueueFAQ{At: e.now.Unix()})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM faq_entries`); n != 1 {
		t.Fatal("duplicate entry")
	}

	// New information in a source thread: stale now, rewritten at the batch.
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 150, PostID: 200, UserID: 5, Body: "Still rattling after the swap.", At: e.now.Unix()})
	if n := e.count(`SELECT COUNT(*) FROM faq_entries WHERE stale = 1`); n != 1 {
		t.Fatal("entry not marked stale")
	}
	e.now = e.now.Add(time.Hour)
	e.apply(&cmd.QueueFAQ{At: e.now.Unix()})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM faq_entries WHERE answer LIKE '%still rattles%' AND stale = 0`); n != 1 {
		t.Fatal("entry not rewritten")
	}
	if n := e.count(`SELECT COUNT(*) FROM faq_history`); n != 2 {
		t.Fatal("history not kept")
	}
	// Roll back to the first version.
	var entry, first int64
	db, _ := e.st.Group(1)
	db.QueryRow(`SELECT entry_id, MIN(id) FROM faq_history`).Scan(&entry, &first)
	e.apply(&cmd.RollbackFAQEntry{GroupID: 1, EntryID: entry, HistoryID: first, By: 5, At: e.now.Unix()})
	if n := e.count(`SELECT COUNT(*) FROM faq_entries WHERE answer NOT LIKE '%still rattles%'`); n != 1 {
		t.Fatal("rollback")
	}

	// Locked: the rewrite waits as a suggestion.
	en, _ := e.st.Entry(1, entry)
	e.apply(&cmd.EditFAQEntry{GroupID: 1, EntryID: entry, TopicID: en.TopicID, Question: en.Question, Answer: en.Answer, Locked: true, By: 5, At: e.now.Unix()})
	e.apply(&cmd.AddFAQComment{GroupID: 1, CommentID: 160, EntryID: entry, UserID: 5, Body: "Still rattling on my 2023.", At: e.now.Unix()})
	e.now = e.now.Add(time.Hour)
	e.apply(&cmd.QueueFAQ{At: e.now.Unix()})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM faq_entries WHERE suggestion LIKE '%still rattles%' AND answer NOT LIKE '%still rattles%'`); n != 1 {
		t.Fatal("a locked entry was rewritten instead of getting a suggestion")
	}

	// A repeat question linked to a source thread joins the entry, and sinks
	// in the Active feed (post 100 is well answered).
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 300, UserID: 5, Title: "Why does my fridge fan rattle?", Body: "Rattle.", At: e.now.Unix()})
	e.apply(&cmd.AddLink{GroupID: 1, PostA: 300, PostB: 100, Source: "author", By: 5, NoteA: 301, NoteB: 302, At: e.now.Unix()})
	if n := e.count(`SELECT COUNT(*) FROM faq_sources WHERE post_id = 300`); n != 1 {
		t.Fatal("repeat question didn't join the entry")
	}
	if n := e.count(`SELECT sink FROM posts WHERE id = 300`); n != cmd.FeedSink {
		t.Fatal("repeat not weighted down in the feed")
	}

	// The weekly outline pass: with Awning, Fridge, Fridges, Tires, the
	// fake model merges 2 into 1 and renames 3. Topics a mod named are
	// left alone.
	e.apply(&cmd.CreateTopic{GroupID: 1, TopicID: 11, Title: "Awning", At: at})
	e.apply(&cmd.CreateTopic{GroupID: 1, TopicID: 12, Title: "Fridges", At: at})
	e.apply(&cmd.CreateTopic{GroupID: 1, TopicID: 13, Title: "Tires", By: 5, At: at})
	e.now = e.now.Add(time.Hour)
	e.apply(&cmd.QueueFAQ{Weekly: true, At: e.now.Unix()})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM faq_topics WHERE title = 'Fridge' AND merged_into = 11`); n != 1 {
		t.Fatal("merge")
	}
	if n := e.count(`SELECT COUNT(*) FROM faq_entries WHERE topic_id = 11`); n != 1 {
		t.Fatal("entry didn't follow its topic's merge")
	}
	if n := e.count(`SELECT COUNT(*) FROM faq_topics WHERE id = 12 AND title = 'Tires and wheels'`); n != 1 {
		t.Fatal("rename")
	}
}

// With three or more related threads, a post gets a combined note.
func TestCombinedNote(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	for i := int64(1); i <= 4; i++ {
		e.apply(&cmd.CreatePost{GroupID: 1, PostID: i * 100, UserID: 5, Title: fmt.Sprintf("Solar %d", i), Body: "Panels.", At: at + i})
	}
	for i := int64(2); i <= 4; i++ {
		e.apply(&cmd.AddLink{GroupID: 1, PostA: 100, PostB: i * 100, Source: "mod", By: 5,
			NoteA: i*100 + 1, NoteB: i*100 + 2, CombinedA: 999, CombinedB: i*100 + 3, At: at + 10})
	}
	e.now = e.now.Add(time.Minute)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE id = 999 AND kind = 'combined' AND text LIKE '%[1]%'`); n != 1 {
		t.Fatal("no combined note on the post with three links")
	}
	if n := e.count(`SELECT COUNT(*) FROM note_sources WHERE note_id = 999`); n != 3 {
		t.Fatal("combined note sources")
	}
	// Unlinking one drops it below three: the combined note goes.
	e.apply(&cmd.RemoveLink{GroupID: 1, PostA: 100, PostB: 400, By: 5, ByMod: true, At: e.now.Unix()})
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE id = 999 AND state = 'active'`); n != 0 {
		t.Fatal("combined note kept below the threshold")
	}
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE kind = 'link' AND state = 'removed'`); n != 2 {
		t.Fatal("unlinking didn't take down both link notes")
	}
}

// Outside pages: read through the restricted fetcher, summarized, shown;
// Facebook only with a description; a removal request is honored at once.
func TestSources(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bulletin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Service bulletin</title></head><body><article><p>Fan revised in 2021.</p><a href="/b2">next</a></article></body></html>`))
	})
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><a href="/bulletin">one</a><a href="/b2">two</a><a href="https://elsewhere.example/">off</a></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newEnv(t)
	f := fetch.New()
	f.AllowPrivate = true
	e.w.Fetch = f
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Fridge fan", Body: "Rattle.", At: at})

	// A member's link from a site the mods haven't allowed waits for them.
	id := e.apply(&cmd.AddSource{GroupID: 1, SourceID: 900, URL: srv.URL + "/bulletin", Via: cmd.ViaMember, AddedBy: 5, PostID: 100, At: at})
	if id != int64(900) || e.count(`SELECT COUNT(*) FROM sources WHERE id = 900 AND status = 'pending'`) != 1 {
		t.Fatal("member link on an unknown site should wait")
	}
	// Approved: read and summarized.
	e.apply(&cmd.ApproveSources{GroupID: 1, SourceIDs: []int64{900}, By: 5, At: at})
	e.drain()
	s, _ := e.st.Source(1, 900)
	if s.Summary == "" || s.Title != "Fan bulletin" || !s.Shown() {
		t.Fatalf("source: %+v", s)
	}
	hits, _ := e.st.SearchKind(1, "source", `"revised"`, 5)
	if len(hits) != 1 {
		t.Fatal("source not in search")
	}

	// Facebook: never read, so a description is required.
	if _, err := e.log.Apply(&cmd.AddSource{GroupID: 1, SourceID: 901, URL: "https://www.facebook.com/groups/x/posts/1", Via: cmd.ViaMember, AddedBy: 5, At: at}); err == nil {
		t.Fatal("Facebook link without a description accepted")
	}
	e.apply(&cmd.AddSource{GroupID: 1, SourceID: 901, URL: "https://www.facebook.com/groups/x/posts/1", Summary: "A member reports the fan swap fixed it.", Via: cmd.ViaMember, AddedBy: 5, At: at})
	if n := e.count(`SELECT COUNT(*) FROM sources WHERE id = 901 AND status = 'active' AND via = 'described'`); n != 1 {
		t.Fatal("described link")
	}
	if n := e.count(`SELECT COUNT(*) FROM jobs WHERE ref_id = 901`); n != 0 {
		t.Fatal("a described page was queued to be read")
	}

	// Seeding from a list page: its links on the same site, waiting for approval.
	e.apply(&cmd.AddSource{GroupID: 1, SourceID: 902, URL: srv.URL + "/list", Via: cmd.ViaSeed, List: true, AddedBy: 5, At: at})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM sources WHERE via = 'seed' AND is_list = 0`); n != 1 {
		t.Fatalf("%d seeded pages, want 1 (the new same-site link; /bulletin is known already)", n)
	}

	// A removal request takes it down at once, summary and all.
	e.apply(&cmd.RemoveSource{GroupID: 1, SourceID: 900, Request: true, At: at})
	s, _ = e.st.Source(1, 900)
	if s.Status != "removed" || s.Summary != "" {
		t.Fatalf("after removal: %+v", s)
	}
	if hits, _ := e.st.SearchKind(1, "source", `"revised"`, 5); len(hits) != 0 {
		t.Fatal("removed source still in search")
	}
	// Re-adding the same page doesn't bring it back.
	e.apply(&cmd.AddSource{GroupID: 1, SourceID: 903, URL: srv.URL + "/bulletin", Via: cmd.ViaMod, AddedBy: 5, At: at})
	if s, _ := e.st.Source(1, 900); s.Status != "removed" {
		t.Fatal("removal didn't stick")
	}
	_ = strings.Contains
	_ = context.Background
}
