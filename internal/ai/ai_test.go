package ai

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/store"
)

// fakeLLM answers each task with something plausible, so the tests check
// the plumbing around the model: queueing, versions, placement, privacy.
type fakeLLM struct {
	mu      sync.Mutex
	prompts []string
}

func (f *fakeLLM) Call(ctx context.Context, req Request, out any) (Usage, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, req.Prompt)
	f.mu.Unlock()
	var answer string
	switch {
	case strings.Contains(req.System, digestTask):
		answer = `{"digest": "Members discuss a rattling fridge fan."}`
	case strings.Contains(req.System, matchTask):
		// The first item, plus any sister-group post; a repeat and a
		// nonsense number, both ignored.
		picks := []string{"1", "1", "42"}
		for _, line := range strings.Split(req.Prompt, "\n") {
			if n, rest, ok := strings.Cut(line, ". In the "); ok && rest != "" {
				picks = append(picks, n)
			}
		}
		answer = `{"same_topic": [` + strings.Join(picks, ", ") + `]}`
	case strings.Contains(req.System, noteTask):
		if strings.Contains(req.Prompt, "CURRENT NOTE") {
			answer = `{"changed": false, "note": ""}`
		} else {
			answer = `{"changed": true, "note": "\"Owners report a 92mm fan swap stopped the rattle.\""}`
		}
	case strings.Contains(req.System, expandTask):
		answer = `{"phrases": ["fridge fan", "rattle"]}`
	case strings.Contains(req.System, summaryTask):
		answer = `{"summary": "Most report the fan bearing; two found a loose screw.", "useful": [5, 99],
			"tangents": [{"from": 2, "to": 4, "about": "tire pressure"}], "superseded": [{"n": 1, "by": 6}]}`
	case strings.Contains(req.System, combinedTask):
		answer = `{"changed": true, "note": "Owners report the fan swap [1] and a loose screw [2]."}`
	case strings.Contains(req.System, faqNewTask):
		answer = `{"question": "How do owners fix a rattling fridge fan?", "answer": "Owners report replacing the fan stopped the rattle.",
			"topic": 0, "new_topic": "Fridge", "parent": 0, "pages": [1]}`
	case strings.Contains(req.System, faqRewriteTask):
		answer = `{"changed": true, "answer": "Owners report replacing the fan; one says it still rattles."}`
	case strings.Contains(req.System, outlineTask):
		answer = `{"merge": [{"keep": 1, "drop": 2}], "rename": [{"n": 3, "title": "Tires and wheels"}]}`
	case strings.Contains(req.System, sourceTask):
		answer = `{"title": "Fan bulletin", "summary": "The bulletin says the fridge fan was revised in 2021."}`
	case strings.Contains(req.System, moderateTask):
		// Tests put these words in an item to get each verdict.
		switch {
		case strings.Contains(req.Prompt, "BUY CHEAP"):
			answer = `{"verdict": "violation", "category": "spam", "reason": "An ad for pills."}`
		case strings.Contains(req.Prompt, "you idiot"):
			answer = `{"verdict": "borderline", "category": "mean", "reason": "Calls another member an idiot."}`
		default:
			answer = `{"verdict": "clear", "category": "", "reason": ""}`
		}
	case strings.Contains(req.System, topicsTask):
		answer = `{"topics": [1]}`
	case strings.Contains(req.System, pickTask):
		answer = `{"cards": [{"n": 1, "statement": "Owners report the stock fan's bearing was the cause."}, {"n": 99, "statement": "made up"}]}`
	default:
		answer = `{}`
	}
	return Usage{InputTokens: 100, OutputTokens: 10}, json.Unmarshal([]byte(answer), out)
}

type env struct {
	t   *testing.T
	st  *store.Store
	log *cluster.Local
	llm *fakeLLM
	eng *Engine
	w   *Worker
	now time.Time
}

func newEnv(t *testing.T) *env {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lg, err := cluster.NewLocal(st)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, log: lg, llm: &fakeLLM{}, now: time.Unix(1_800_000_000, 0)}
	e.eng = &Engine{LLM: e.llm, Store: st, Meter: &Meter{}}
	e.w = &Worker{Engine: e.eng, Log: lg, IDs: ids.New(7), Name: "studio", Now: func() time.Time { return e.now }}
	e.apply(&cmd.CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: e.now.Unix()})
	e.apply(&cmd.JoinGroup{GroupID: 1, UserID: 5, At: e.now.Unix()})
	return e
}

func (e *env) apply(c cmd.Command) any {
	e.t.Helper()
	v, err := e.log.Apply(c)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// drain runs the worker until nothing is due at the current time.
func (e *env) drain() int {
	total := 0
	for i := 0; i < 10; i++ {
		n, err := e.w.RunOnce(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total
}

func (e *env) count(q string, args ...any) int {
	db, _ := e.st.Group(1)
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestJobsLinkAndDigest(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Fridge fan rattle", Body: "Rattles at 40 mph.", At: at})
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 200, UserID: 5, Title: "Noisy fridge fan", Body: "Fan rattle on the highway.", At: at + 60})
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 300, UserID: 5, Title: "Tire pressure", Body: "What do you run?", At: at + 120})

	// Checks run at once; digests wait for the thread to settle.
	e.now = e.now.Add(3 * time.Minute)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM post_links WHERE state = 'active'`); n != 1 {
		t.Fatalf("%d links, want 1 (fridge posts only)", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE text LIKE 'Owners report%'`); n != 2 {
		t.Fatalf("%d notes written, want 2 (one each side, quotes trimmed)", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM posts WHERE digest IS NOT NULL`); n != 0 {
		t.Fatal("digests ran before the quiet period")
	}
	e.now = e.now.Add(cmd.QuietPeriod * time.Second)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM posts WHERE digest IS NOT NULL`); n != 3 {
		t.Fatalf("%d digests, want 3", n)
	}

	// No handles or ids reach the model: threads are "OP" and "commenter N".
	for _, p := range e.llm.prompts {
		if strings.Contains(p, "UserID") || strings.Contains(p, "user 5") {
			t.Fatalf("identity in a prompt: %s", p)
		}
	}

	// A comment on post 100 makes the note on 200 (written from 100) stale.
	// It's refreshed once things settle; "no change" keeps the text.
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 101, PostID: 100, UserID: 5, Body: "Still rattling.", At: e.now.Unix()})
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE host_post_id = 200 AND stale = 1`); n != 1 {
		t.Fatal("note not marked stale")
	}
	before := e.count(`SELECT updated_at FROM notes WHERE host_post_id = 200`)
	e.now = e.now.Add(time.Hour + time.Minute)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM notes WHERE host_post_id = 200 AND stale = 0`); n != 1 {
		t.Fatal("stale note not refreshed")
	}
	if after := e.count(`SELECT updated_at FROM notes WHERE host_post_id = 200`); after != before {
		t.Fatal("a no-change refresh moved the note's date")
	}

	// Removing the link hides both notes and stops it coming back.
	e.apply(&cmd.RemoveLink{GroupID: 1, PostA: 100, PostB: 200, By: 5, ByMod: true, At: e.now.Unix()})
	e.apply(&cmd.EditPost{GroupID: 1, PostID: 200, EditorID: 5, Title: "Noisy fridge fan (still)", Body: "Fan rattle.", At: e.now.Unix()})
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM post_links WHERE state = 'active'`); n != 0 {
		t.Fatal("a rejected pair was linked again")
	}
}

// A result computed from an older version is dropped, and the job waits to
// run again: a slow worker never overwrites newer content.
func TestStaleResultDropped(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Solar", Body: "Two panels.", At: at})
	jobs, _ := e.st.DueJobs(1, at+cmd.QuietPeriod, 10)
	var digest store.Job
	for _, j := range jobs {
		if j.Kind == cmd.JobDigest {
			digest = j
		}
	}
	if got := e.apply(&cmd.ClaimJob{GroupID: 1, JobID: digest.ID, Worker: "studio", At: at + cmd.QuietPeriod}); got != true {
		t.Fatal("claim failed")
	}
	// Someone else can't take it while the lease holds.
	if got := e.apply(&cmd.ClaimJob{GroupID: 1, JobID: digest.ID, Worker: "mac2", At: at + cmd.QuietPeriod + 1}); got != false {
		t.Fatal("double claim")
	}
	p, _ := e.st.Post(1, 100)
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 101, PostID: 100, UserID: 5, Body: "Which controller?", At: at + 1000})
	e.apply(&cmd.SetDigest{GroupID: 1, JobID: digest.ID, Worker: "studio", PostID: 100, Version: p.ThreadVersion, Digest: "old", At: at + 1001})
	if n := e.count(`SELECT COUNT(*) FROM posts WHERE digest IS NOT NULL`); n != 0 {
		t.Fatal("stale digest was written")
	}
	if n := e.count(`SELECT COUNT(*) FROM jobs WHERE id = ? AND done_at IS NULL AND claimed_by IS NULL`, digest.ID); n != 1 {
		t.Fatal("job not released for another run")
	}
}

func TestAskAndGroupsWithoutAI(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Fridge fan rattle", Body: "Rattles at speed.", At: at})
	cards, err := e.eng.Ask(context.Background(), AskRequest{GroupIDs: []int64{1}, Question: "has anyone fixed the fridge fan rattle?"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].PostID != 100 || cards[0].GroupID != 1 {
		t.Fatalf("cards: %+v", cards)
	}

	// With "Use AI in this group" off, no job runs and search finds nothing
	// to ask the model about.
	e.apply(&cmd.UpdateSettings{GroupID: 1, Set: map[string]any{"ai_enabled": false}, By: 5, At: at})
	e.now = e.now.Add(time.Hour)
	if n := e.drain(); n != 0 {
		t.Fatalf("%d jobs ran in a group with AI off", n)
	}
	cards, _ = e.eng.Ask(context.Background(), AskRequest{GroupIDs: []int64{1}, Question: "fridge fan"})
	if len(cards) != 0 {
		t.Fatal("search used a group with AI off")
	}
}

// Searches go ahead of background jobs waiting for the model.
func TestGatePrefersSearches(t *testing.T) {
	var g gate
	ctx := context.Background()
	g.enter(ctx, false) // a job is running
	g.searchStart()
	order := make(chan string, 2)
	go func() { g.enter(ctx, false); order <- "job"; g.leave() }()
	go func() { g.enter(ctx, true); order <- "search"; g.leave(); g.searchDone() }()
	time.Sleep(20 * time.Millisecond)
	g.leave()
	if first := <-order; first != "search" {
		t.Fatalf("%s went first", first)
	}
	<-order

	// A search that gives up while waiting doesn't block anything.
	g.enter(ctx, false)
	tctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := g.enter(tctx, true); err == nil {
		t.Fatal("entered while busy")
	}
	g.leave()
}
