package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The model tasks behind M3: summary notes, combined notes, FAQ entries,
// the outline pass, outside-page summaries, and topic tags. Each is one
// call with a fixed prompt and a JSON schema; everything around the call
// (what to read, what to keep, what's allowed) is plain code.

// More budgets, in characters (see tasks.go).
const (
	summaryBudget = 40000 // a long thread's comments
	sourceBudget  = 16000 // an outside page's text
	faqPerThread  = 1200  // each thread's digest in a FAQ call
)

// SummaryResult is a summary job's output, in comment ids.
type SummaryResult struct {
	Text       string
	Covers     []int64
	Useful     []int64
	Tangents   []cmd.Tangent
	Superseded []cmd.Supersede
}

// keepOpen is how many of a thread's latest comments a summary never
// covers: the conversation that's still going stays in view.
const keepOpen = 3

// Summary writes a long thread's summary note and finds its nudges.
func (e *Engine) Summary(ctx context.Context, groupID, postID int64) (*SummaryResult, error) {
	text, ids, err := e.Store.NumberedComments(groupID, postID, summaryBudget)
	if err != nil || len(ids) < cmd.SummaryMin {
		return nil, err
	}
	var out struct {
		Summary  string `json:"summary"`
		Useful   []int  `json:"useful"`
		Tangents []struct {
			From  int    `json:"from"`
			To    int    `json:"to"`
			About string `json:"about"`
		} `json:"tangents"`
		Superseded []struct {
			N  int `json:"n"`
			By int `json:"by"`
		} `json:"superseded"`
	}
	if err := e.call(ctx, "summary", false, Voice+summaryTask, text, summarySchema, &out); err != nil {
		return nil, err
	}
	// Numbers the model gives back are checked against what it was shown;
	// anything out of range is dropped rather than trusted.
	id := func(n int) int64 {
		if n < 1 || n > len(ids) {
			return 0
		}
		return ids[n-1]
	}
	r := &SummaryResult{Text: clean(out.Summary)}
	covered := len(ids)
	if all, _ := e.Store.Comments(groupID, postID); countShown(all) <= covered {
		covered -= keepOpen // the model read the whole thread: leave the latest few open
	}
	r.Covers = ids[:max(covered, 0)]
	for _, n := range out.Useful {
		if c := id(n); c != 0 {
			r.Useful = append(r.Useful, c)
		}
	}
	for _, t := range out.Tangents {
		if from, to := id(t.From), id(t.To); from != 0 && to != 0 && t.To-t.From >= 2 {
			r.Tangents = append(r.Tangents, cmd.Tangent{From: from, To: to, About: clean(t.About)})
		}
	}
	for _, s := range out.Superseded {
		if c, by := id(s.N), id(s.By); c != 0 && by != 0 && s.By > s.N {
			r.Superseded = append(r.Superseded, cmd.Supersede{Comment: c, By: by})
		}
	}
	return r, nil
}

func countShown(cs []store.Comment) int {
	n := 0
	for _, c := range cs {
		if c.Status == "visible" || c.Status == "flagged" {
			n++
		}
	}
	return n
}

// Combined writes the combined note at the top of host from its sources:
// other threads (by digest) and outside pages (by summary), numbered so
// the note can cite them as [n].
func (e *Engine) Combined(ctx context.Context, groupID, host int64, sources []store.NoteSource, current string) (string, bool, error) {
	hostText, hp, err := e.Store.ThreadText(groupID, host, noteHostLen)
	if err != nil || hp == nil {
		return "", false, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "THIS THREAD:\n%s\n\nSOURCES:\n", hostText)
	for i, s := range sources {
		line, err := e.sourceLine(groupID, s)
		if err != nil {
			return "", false, err
		}
		fmt.Fprintf(&b, "[%d] %s\n\n", i+1, line)
	}
	if current != "" {
		fmt.Fprintf(&b, "CURRENT NOTE:\n%s\n", current)
	}
	var out struct {
		Changed bool   `json:"changed"`
		Note    string `json:"note"`
	}
	if err := e.call(ctx, "note", false, Voice+combinedTask, b.String(), noteSchema, &out); err != nil {
		return "", false, err
	}
	text := clean(out.Note)
	if current == "" {
		return text, text != "", nil
	}
	return text, out.Changed && text != "", nil
}

// sourceLine is one numbered source for a combined note or FAQ call.
func (e *Engine) sourceLine(groupID int64, s store.NoteSource) (string, error) {
	if s.SourceID != 0 {
		src, err := e.Store.Source(groupID, s.SourceID)
		if err != nil || src == nil || !src.Shown() {
			return "(no longer available)", err
		}
		return pageLine(src), nil
	}
	p, err := e.Store.Post(s.GroupID, s.PostID)
	if err != nil || p == nil || !shown(p.Status) {
		return "(no longer available)", err
	}
	return "Thread " + summaryLine(p), nil
}

func pageLine(s *store.Source) string {
	when := ""
	if s.PublishedAt != 0 {
		when = " (" + time.Unix(s.PublishedAt, 0).UTC().Format("Jan 2006") + ")"
	}
	return fmt.Sprintf("Outside page on %s, %q%s: %s", s.Site, s.Title, when, s.Summary)
}

// FAQDraft is a new entry as the model wrote it, in ids.
type FAQDraft struct {
	Question  string
	Answer    string
	TopicID   int64  // an existing topic, or 0
	NewTopic  string // when TopicID is 0
	NewParent int64
	Pages     []int64
}

// FAQNew writes a new entry for a cluster of threads, choosing (or naming)
// its topic.
func (e *Engine) FAQNew(ctx context.Context, groupID int64, posts []store.Post, pages []store.Source, topics []store.Topic) (*FAQDraft, error) {
	var b strings.Builder
	b.WriteString("TOPICS:\n")
	b.WriteString(topicList(topics))
	b.WriteString("\nTHREADS:\n")
	for i, p := range posts {
		fmt.Fprintf(&b, "%d. %s\n\n", i+1, faqThreadLine(&p))
	}
	if len(pages) > 0 {
		b.WriteString("PAGES:\n")
		for i, s := range pages {
			fmt.Fprintf(&b, "%d. %s\n\n", i+1, pageLine(&s))
		}
	}
	var out struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
		Topic    int    `json:"topic"`
		NewTopic string `json:"new_topic"`
		Parent   int    `json:"parent"`
		Pages    []int  `json:"pages"`
	}
	if err := e.call(ctx, "faq", false, Voice+faqNewTask, b.String(), faqNewSchema, &out); err != nil {
		return nil, err
	}
	d := &FAQDraft{Question: clean(out.Question), Answer: clean(out.Answer)}
	if out.Topic >= 1 && out.Topic <= len(topics) {
		d.TopicID = topics[out.Topic-1].ID
	} else {
		d.NewTopic = clean(out.NewTopic)
		if d.NewTopic == "" {
			d.NewTopic = "General"
		}
		// Only a main topic can be a parent: the outline is two levels.
		if out.Parent >= 1 && out.Parent <= len(topics) && topics[out.Parent-1].ParentID == 0 {
			d.NewParent = topics[out.Parent-1].ID
		}
	}
	for _, n := range out.Pages {
		if n >= 1 && n <= len(pages) {
			d.Pages = append(d.Pages, pages[n-1].ID)
		}
	}
	return d, nil
}

// topicList numbers the outline for the model, subtopics as "Parent >
// Child". Numbered in the order given, which is the order the answer's
// numbers are read back in.
func topicList(topics []store.Topic) string {
	title := map[int64]string{}
	for _, t := range topics {
		title[t.ID] = t.Title
	}
	var b strings.Builder
	for i, t := range topics {
		name := t.Title
		if t.ParentID != 0 {
			name = title[t.ParentID] + " > " + t.Title
		}
		fmt.Fprintf(&b, "%d. %s (%d entries)\n", i+1, name, t.Entries)
	}
	if len(topics) == 0 {
		b.WriteString("(none yet)\n")
	}
	return b.String()
}

func faqThreadLine(p *store.Post) string {
	d := digestOrOpening(p)
	if r := []rune(d); len(r) > faqPerThread {
		d = string(r[:faqPerThread]) + "…"
	}
	return fmt.Sprintf("%s (%s, %d comments): %s", p.Title, time.Unix(p.CreatedAt, 0).UTC().Format("Jan 2006"), p.CommentCount, d)
}

// FAQRewrite brings an entry up to date with its sources. It returns
// changed=false when there's nothing meaningfully new.
func (e *Engine) FAQRewrite(ctx context.Context, entry *store.Entry, posts []store.Post, pages []store.Source, comments []store.FAQComment) (string, bool, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "ENTRY:\nQuestion: %s\nAnswer: %s\n\nTHREADS:\n", entry.Question, entry.Answer)
	for i, p := range posts {
		fmt.Fprintf(&b, "%d. %s\n\n", i+1, faqThreadLine(&p))
	}
	if len(pages) > 0 {
		b.WriteString("PAGES:\n")
		for i, s := range pages {
			fmt.Fprintf(&b, "%d. %s\n\n", i+1, pageLine(&s))
		}
	}
	if len(comments) > 0 {
		b.WriteString("COMMENTS:\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "- a member, %s: %s\n", time.Unix(c.CreatedAt, 0).UTC().Format("2006-01-02"), oneLine(c.Body, 800))
		}
	}
	var out struct {
		Changed bool   `json:"changed"`
		Answer  string `json:"answer"`
	}
	if err := e.call(ctx, "faq", false, Voice+faqRewriteTask, b.String(), faqRewriteSchema, &out); err != nil {
		return "", false, err
	}
	ans := clean(out.Answer)
	return ans, out.Changed && ans != "", nil
}

// Outline is the weekly pass over the topic titles: merges and renames.
func (e *Engine) Outline(ctx context.Context, topics []store.Topic) ([]cmd.TopicMerge, []cmd.TopicRename, error) {
	var out struct {
		Merge []struct {
			Keep int `json:"keep"`
			Drop int `json:"drop"`
		} `json:"merge"`
		Rename []struct {
			N     int    `json:"n"`
			Title string `json:"title"`
		} `json:"rename"`
	}
	if err := e.call(ctx, "faq", false, Voice+outlineTask, "TOPICS:\n"+topicList(topics), outlineSchema, &out); err != nil {
		return nil, nil, err
	}
	ok := func(n int) bool { return n >= 1 && n <= len(topics) }
	var merges []cmd.TopicMerge
	for _, m := range out.Merge {
		if ok(m.Keep) && ok(m.Drop) && m.Keep != m.Drop {
			merges = append(merges, cmd.TopicMerge{Keep: topics[m.Keep-1].ID, Drop: topics[m.Drop-1].ID})
		}
	}
	var renames []cmd.TopicRename
	for _, r := range out.Rename {
		if ok(r.N) && clean(r.Title) != "" {
			renames = append(renames, cmd.TopicRename{ID: topics[r.N-1].ID, Title: clean(r.Title)})
		}
	}
	return merges, renames, nil
}

// SummarizeSource writes an outside page's title and summary from its
// text. The text is used for this call only and never stored.
func (e *Engine) SummarizeSource(ctx context.Context, site, title, text string) (string, string, error) {
	if r := []rune(text); len(r) > sourceBudget {
		text = string(r[:sourceBudget])
	}
	var out struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	prompt := fmt.Sprintf("SITE: %s\nTITLE: %s\nTEXT:\n%s", site, title, text)
	if err := e.call(ctx, "source", false, Voice+sourceTask, prompt, sourceSchema, &out); err != nil {
		return "", "", err
	}
	t := clean(out.Title)
	if t == "" {
		t = title
	}
	return t, clean(out.Summary), nil
}

// ChooseTopics tags a post with up to three topics from the outline.
func (e *Engine) ChooseTopics(ctx context.Context, post string, topics []store.Topic) ([]int64, error) {
	if len(topics) == 0 {
		return nil, nil
	}
	var out struct {
		Topics []int `json:"topics"`
	}
	prompt := "TOPICS:\n" + topicList(topics) + "\nPOST:\n" + post
	if err := e.call(ctx, "digest", false, Voice+topicsTask, prompt, topicsSchema, &out); err != nil {
		return nil, err
	}
	var ids []int64
	seen := map[int]bool{}
	for _, n := range out.Topics {
		if n >= 1 && n <= len(topics) && !seen[n] && len(ids) < 3 {
			seen[n] = true
			ids = append(ids, topics[n-1].ID)
		}
	}
	return ids, nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
