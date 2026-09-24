package ai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/fetch"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/store"
)

// Worker runs the background AI jobs on a node that has a model (the
// Studio). It reads the queue from its own copy of the data, claims each
// job through the leader, runs it against its own copy, and submits the
// result as a command carrying the version it read (plan section 9).
type Worker struct {
	Engine *Engine
	Log    cluster.Log // on a follower, Apply forwards to the leader
	IDs    *ids.Generator
	Name   string // this node's id, recorded as the job's claimer
	Now    func() time.Time
	Fetch  *fetch.Fetcher // reads outside pages; nil = outside pages wait
}

// Run works the queue until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	for {
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("ai worker: %v", err)
		}
		if n > 0 {
			continue // there may be more
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// RunOnce runs the jobs that are due right now, and returns how many ran.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	groups, err := w.Engine.Store.GroupFileIDs()
	if err != nil {
		return 0, err
	}
	ran := 0
	for _, g := range groups {
		st, err := w.Engine.Store.GroupSettings(g)
		if err != nil {
			return ran, err
		}
		if st == nil || !st.AIEnabled {
			continue // "Use AI in this group" is off: no AI touches it
		}
		jobs, err := w.Engine.Store.DueJobs(g, w.Now().Unix(), 20)
		if err != nil {
			return ran, err
		}
		for _, j := range jobs {
			if ctx.Err() != nil {
				return ran, nil
			}
			got, err := w.Log.Apply(&cmd.ClaimJob{GroupID: g, JobID: j.ID, Worker: w.Name, At: w.Now().Unix()})
			if err != nil {
				return ran, err
			}
			if got != true {
				continue // another worker has it, or it moved
			}
			ran++
			// A job gets a few minutes, inside its lease.
			jctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			result, err := w.RunJob(jctx, g, j)
			cancel()
			if err == nil {
				_, err = w.Log.Apply(result)
			}
			if err != nil {
				log.Printf("ai job %s %d in group %d: %v", j.Kind, j.RefID, g, err)
				w.Log.Apply(&cmd.FailJob{GroupID: g, JobID: j.ID, Worker: w.Name, Error: err.Error(), At: w.Now().Unix()})
			}
		}
	}
	return ran, nil
}

// RunJob does one job's model work and returns the command that records
// its result.
func (w *Worker) RunJob(ctx context.Context, groupID int64, j store.Job) (cmd.Command, error) {
	st := w.Engine.Store
	switch j.Kind {
	case cmd.JobDigest:
		p, err := st.Post(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		res := &cmd.SetDigest{GroupID: groupID, JobID: j.ID, Worker: w.Name, PostID: j.RefID}
		if p == nil || !shown(p.Status) {
			if p != nil {
				res.Version = p.ThreadVersion
			}
			return w.stamp(res), nil // nothing to digest; closes the job
		}
		res.Version = p.ThreadVersion // read before the text, so a change meanwhile makes this stale, not wrong
		if res.Digest, err = w.Engine.Digest(ctx, groupID, j.RefID); err != nil {
			return nil, err
		}
		// Topic tags ride along with the digest, once the group has an
		// outline to choose from.
		tree, err := st.Topics(groupID)
		if err != nil {
			return nil, err
		}
		if topics := store.FlatTopics(tree); len(topics) > 0 && p.ContinuesID == 0 {
			res.Topics, err = w.Engine.ChooseTopics(ctx, postForMatch(p), topics)
			res.TopicsDone = err == nil
			if err != nil {
				return nil, err
			}
		}
		return w.stamp(res), nil

	case cmd.JobCheck:
		p, err := st.Post(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		res := &cmd.SetCheck{GroupID: groupID, JobID: j.ID, Worker: w.Name, PostID: j.RefID}
		if p == nil || !shown(p.Status) {
			if p != nil {
				res.Version = p.Version
			}
			return w.stamp(res), nil
		}
		res.Version = p.Version
		cands, err := w.candidates(groupID, p)
		if err != nil {
			return nil, err
		}
		matches, err := w.Engine.Match(ctx, postForMatch(p), cands)
		if err != nil {
			return nil, err
		}
		// A post rarely has more than a few true matches of each kind;
		// more is noise.
		for _, m := range matches {
			switch {
			case m.Kind == kindPost && len(res.Links) < 3:
				res.Links = append(res.Links, cmd.CheckLink{Other: m.ID, NoteHere: w.IDs.Next(), NoteThere: w.IDs.Next(),
					CombinedHere: w.IDs.Next(), CombinedThere: w.IDs.Next()})
			case m.Kind == kindFAQ && len(res.Entries) < 2:
				res.Entries = append(res.Entries, m.ID)
			case m.Kind == kindPage && len(res.Sources) < 3:
				res.Sources = append(res.Sources, m.ID)
			}
		}
		return w.stamp(res), nil

	case cmd.JobNote:
		v, err := st.NoteVersion(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		res := &cmd.SetNote{GroupID: groupID, JobID: j.ID, Worker: w.Name, NoteID: j.RefID, Version: v, NoChange: true}
		n, err := st.Note(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		if n == nil {
			return w.stamp(res), nil // removed meanwhile
		}
		var text string
		var changed bool
		switch n.Kind {
		case "combined":
			srcs, err := st.NoteSources(groupID, n.ID)
			if err != nil {
				return nil, err
			}
			text, changed, err = w.Engine.Combined(ctx, groupID, n.HostPostID, srcs, n.Text)
			if err != nil {
				return nil, err
			}
		case "link":
			if n.SourcePostID == 0 || n.SourceGroupID != groupID {
				return w.stamp(res), nil // not ours to write
			}
			text, changed, err = w.Engine.Note(ctx, groupID, n.HostPostID, n.SourcePostID, n.Text)
			if err != nil {
				return nil, err
			}
		default:
			return w.stamp(res), nil // summaries are written by their own job
		}
		res.Text, res.NoChange = text, !changed
		return w.stamp(res), nil

	case cmd.JobSummary:
		p, err := st.Post(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		res := &cmd.SetSummary{GroupID: groupID, JobID: j.ID, Worker: w.Name, PostID: j.RefID, NoteID: w.IDs.Next()}
		if p == nil || !shown(p.Status) {
			if p != nil {
				res.Version = p.ThreadVersion
			}
			return w.stamp(res), nil
		}
		res.Version = p.ThreadVersion
		sum, err := w.Engine.Summary(ctx, groupID, p.ID)
		if err != nil {
			return nil, err
		}
		if sum != nil {
			res.Text, res.Covers, res.Useful, res.Tangents, res.Superseded = sum.Text, sum.Covers, sum.Useful, sum.Tangents, sum.Superseded
		}
		return w.stamp(res), nil

	case cmd.JobFAQNew, cmd.JobFAQRewrite, cmd.JobOutline:
		return w.runFAQ(ctx, groupID, j)

	case cmd.JobSource, cmd.JobSeed:
		return w.runSource(ctx, groupID, j)
	}
	return nil, fmt.Errorf("unknown job kind %q", j.Kind)
}

// stamp fills in the time a result is submitted.
func (w *Worker) stamp(c cmd.Command) cmd.Command {
	at := w.Now().Unix()
	switch r := c.(type) {
	case *cmd.SetDigest:
		r.At = at
	case *cmd.SetCheck:
		r.At = at
	case *cmd.SetNote:
		r.At = at
	case *cmd.SetSummary:
		r.At = at
	case *cmd.CreateFAQEntry:
		r.At = at
	case *cmd.SetFAQAnswer:
		r.At = at
	case *cmd.TidyTopics:
		r.At = at
	case *cmd.SetSource:
		r.At = at
	case *cmd.AddSeeds:
		r.At = at
	}
	return c
}

// Kinds of match candidate.
const (
	kindPost = "post"
	kindFAQ  = "faq"
	kindPage = "page"
)

// candidates finds what a post is most likely about the same thing as,
// with plain full-text search (no model): other posts, FAQ entries, and
// outside pages, found by the words of the post's title and opening and
// ranked by BM25. The model then only has to judge a short list.
func (w *Worker) candidates(groupID int64, p *store.Post) ([]Candidate, error) {
	st := w.Engine.Store
	terms := store.Terms(p.Title + " " + p.Body)
	if len(terms) > 25 {
		terms = terms[:25]
	}
	query := store.FTSQuery(strings.Join(terms, " "))
	hits, err := st.Search(groupID, query, 12, true)
	if err != nil {
		return nil, err
	}
	paired, err := st.LinkPairs(groupID, p.ID)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, h := range hits {
		if h.PostID == p.ID || paired[h.PostID] || len(out) == 8 {
			continue
		}
		o, err := st.Post(groupID, h.PostID)
		if err != nil {
			return nil, err
		}
		if o == nil || o.ContinuesID == p.ID || p.ContinuesID == o.ID {
			continue // an update under this post is already part of it
		}
		out = append(out, Candidate{ID: o.ID, Kind: kindPost, Text: summaryLine(o)})
	}
	// FAQ entries it doesn't already belong to, and outside pages not yet
	// on it: a few of each.
	in, err := st.EntriesForPost(groupID, p.ID)
	if err != nil {
		return nil, err
	}
	member := map[int64]bool{}
	for _, e := range in {
		member[e.ID] = true
	}
	faqs, err := st.SearchKind(groupID, "faq", query, 3)
	if err != nil {
		return nil, err
	}
	for _, h := range faqs {
		if e, err := st.Entry(groupID, h.ID); err == nil && e != nil && !member[e.ID] {
			out = append(out, Candidate{ID: e.ID, Kind: kindFAQ, Text: "FAQ entry: " + e.Question + " " + oneLine(e.Answer, candidateLen)})
		}
	}
	attached, err := st.SourcesForPost(groupID, p.ID)
	if err != nil {
		return nil, err
	}
	on := map[int64]bool{}
	for _, s := range attached {
		on[s.ID] = true
	}
	pages, err := st.SearchKind(groupID, "source", query, 3)
	if err != nil {
		return nil, err
	}
	for _, h := range pages {
		if s, err := st.Source(groupID, h.ID); err == nil && s != nil && s.Shown() && !on[s.ID] {
			out = append(out, Candidate{ID: s.ID, Kind: kindPage, Text: oneLine(pageLine(s), candidateLen)})
		}
	}
	return out, nil
}

func postForMatch(p *store.Post) string {
	body := p.Body
	if len(body) > matchPostLen {
		body = body[:matchPostLen] + "…"
	}
	return fmt.Sprintf("%s (%s)\n%s", p.Title, time.Unix(p.CreatedAt, 0).UTC().Format("Jan 2006"), body)
}

// summaryLine is a post as one short entry: title, date, and its digest,
// or its opening if it has no digest yet.
func summaryLine(p *store.Post) string {
	about := p.Digest
	if about == "" {
		about = strings.Join(strings.Fields(p.Body), " ")
		if r := []rune(about); len(r) > candidateLen {
			about = string(r[:candidateLen]) + "…"
		}
	}
	return fmt.Sprintf("%s (%s): %s", p.Title, time.Unix(p.CreatedAt, 0).UTC().Format("Jan 2006"), about)
}

func shown(status string) bool { return status == "visible" || status == "flagged" }

// ErrNoWorker means no node with a model is reachable.
var ErrNoWorker = errors.New("no AI worker available")
