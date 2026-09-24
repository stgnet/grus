package ai

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/store"
)

// Engine runs the model tasks on this node. It holds the model, the node's
// copy of the data, and the gate that keeps one call at a time running on
// the model (a local model server does one thing at a time well; more just
// queue inside it where we can't prioritize them).
type Engine struct {
	LLM   LLM
	Store *store.Store
	Meter *Meter
	gate  gate

	mu     sync.Mutex
	avgAsk float64 // rolling seconds per search, for routing
}

// call runs one model call through the gate and counts it. interactive
// calls (search) go ahead of waiting background jobs.
func (e *Engine) call(ctx context.Context, purpose string, interactive bool, system, prompt string, schema map[string]any, out any) error {
	if err := e.gate.enter(ctx, interactive); err != nil {
		return err
	}
	defer e.gate.leave()
	u, err := e.LLM.Call(ctx, Request{System: system, Prompt: prompt, Schema: schema}, out)
	e.Meter.Add(purpose, u, err != nil)
	return err
}

// Waiting reports how many search calls are queued or running, for /health.
func (e *Engine) Waiting() int { return e.gate.load() }

// Budgets for how much thread text goes into one call, in characters
// (roughly 4 per token). They keep a call well inside a 16k-token context
// and predictable in time.
const (
	digestBudget = 24000
	noteHostLen  = 4000
	noteOtherLen = 16000
	matchPostLen = 3000
	candidateLen = 400
)

// Digest writes a thread's digest.
func (e *Engine) Digest(ctx context.Context, groupID, postID int64) (string, error) {
	text, p, err := e.Store.ThreadText(groupID, postID, digestBudget)
	if err != nil || p == nil {
		return "", err
	}
	var out struct {
		Digest string `json:"digest"`
	}
	err = e.call(ctx, "digest", false, Voice+digestTask, text, digestSchema, &out)
	return clean(out.Digest), err
}

// Candidate is another post offered to the match step.
type Candidate struct {
	ID   int64
	Text string // title, date, and digest or opening
}

// Match picks which candidates are about the same thing as the post.
func (e *Engine) Match(ctx context.Context, post string, cands []Candidate) ([]int64, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "NEW POST:\n%s\n\nOTHER POSTS:\n", post)
	for i, c := range cands {
		// Numbered 1..n rather than by id: small models copy short numbers
		// reliably and long ones not.
		fmt.Fprintf(&b, "%d. %s\n", i+1, c.Text)
	}
	var out struct {
		SameTopic []int `json:"same_topic"`
	}
	if err := e.call(ctx, "check", false, Voice+matchTask, b.String(), matchSchema, &out); err != nil {
		return nil, err
	}
	var ids []int64
	seen := map[int]bool{}
	for _, n := range out.SameTopic {
		if n >= 1 && n <= len(cands) && !seen[n] {
			seen[n] = true
			ids = append(ids, cands[n-1].ID)
		}
	}
	return ids, nil
}

// Note writes (or refreshes) the note on host that points at other.
// It returns changed=false when the model sees nothing new to say.
func (e *Engine) Note(ctx context.Context, groupID, host, other int64, current string) (text string, changed bool, err error) {
	hostText, hp, err := e.Store.ThreadText(groupID, host, noteHostLen)
	if err != nil || hp == nil {
		return "", false, err
	}
	otherText, op, err := e.Store.ThreadText(groupID, other, noteOtherLen)
	if err != nil || op == nil {
		return "", false, err
	}
	when := "older"
	if op.CreatedAt > hp.CreatedAt {
		when = "newer"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "THIS THREAD:\n%s\n\nOTHER THREAD (%s than this one):\n%s\n", hostText, when, otherText)
	if current != "" {
		fmt.Fprintf(&b, "\nCURRENT NOTE:\n%s\n", current)
	}
	var out struct {
		Changed bool   `json:"changed"`
		Note    string `json:"note"`
	}
	if err := e.call(ctx, "note", false, Voice+noteTask, b.String(), noteSchema, &out); err != nil {
		return "", false, err
	}
	text = clean(out.Note)
	if current == "" {
		return text, text != "", nil // a first note is always written
	}
	return text, out.Changed && text != "", nil
}

// Expand turns a question into search phrases.
func (e *Engine) Expand(ctx context.Context, question, prev string) ([]string, error) {
	prompt := "SEARCH: " + question
	if prev != "" {
		prompt = "PREVIOUS SEARCH: " + prev + "\n" + prompt
	}
	var out struct {
		Phrases []string `json:"phrases"`
	}
	if err := e.call(ctx, "ask", true, Voice+expandTask, prompt, expandSchema, &out); err != nil {
		return nil, err
	}
	if len(out.Phrases) > 8 {
		out.Phrases = out.Phrases[:8]
	}
	return out.Phrases, nil
}

// Card is one search result statement: what one thread says about the
// question.
type Card struct {
	GroupID   int64
	PostID    int64
	Statement string
}

// ThreadSummary is what the pick step reads per thread.
type ThreadSummary struct {
	GroupID int64
	PostID  int64
	Title   string
	Date    int64
	Digest  string
}

// Pick ranks threads for a question and states what each says about it.
func (e *Engine) Pick(ctx context.Context, question string, threads []ThreadSummary) ([]Card, error) {
	if len(threads) == 0 {
		return nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "QUESTION: %s\n\nTHREADS:\n", question)
	for i, t := range threads {
		fmt.Fprintf(&b, "%d. %s (%s)\n%s\n\n", i+1, t.Title, time.Unix(t.Date, 0).UTC().Format("Jan 2006"), t.Digest)
	}
	var out struct {
		Cards []struct {
			N         int    `json:"n"`
			Statement string `json:"statement"`
		} `json:"cards"`
	}
	if err := e.call(ctx, "ask", true, Voice+pickTask, b.String(), pickSchema, &out); err != nil {
		return nil, err
	}
	var cards []Card
	seen := map[int]bool{}
	for _, c := range out.Cards {
		if c.N < 1 || c.N > len(threads) || seen[c.N] || len(cards) == 6 {
			continue
		}
		seen[c.N] = true
		t := threads[c.N-1]
		if s := clean(c.Statement); s != "" {
			cards = append(cards, Card{GroupID: t.GroupID, PostID: t.PostID, Statement: s})
		}
	}
	return cards, nil
}

// clean tidies model text: trims, drops wrapping quotes, and caps length.
// It can't make a bad statement good, but it keeps formatting slips (a
// quoted answer, a stray markdown bullet) off the page.
func clean(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"“”")
	s = strings.TrimPrefix(s, "- ")
	s = strings.TrimPrefix(s, "* ")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 1000 {
		s = string(r[:1000])
	}
	return s
}
