package ai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/auth"
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

// sweepEvery is how often the worker looks for sister-group notes whose
// source thread changed.
const sweepEvery = 15 * time.Minute

// Run works the queue until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	var swept time.Time
	for {
		if time.Since(swept) > sweepEvery {
			swept = time.Now()
			if err := w.sweepSisters(ctx); err != nil && ctx.Err() == nil {
				log.Printf("ai worker: sister sweep: %v", err)
			}
		}
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
		// The moderation check (M5), in its own call; see moderate.go.
		v, err := w.Engine.Moderate(ctx, groupID, postText(p), "")
		if err != nil {
			return nil, err
		}
		res.Verdict, res.Category, res.Reason = v.Verdict, v.Category, v.Reason
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
			case m.Kind == kindSister && len(res.Sisters) < 2:
				o, err := st.Post(m.Group, m.ID)
				if err != nil {
					return nil, err
				}
				// Which sides may carry a note is decided here, from
				// site.db as this node sees it, and carried in the command
				// (a group's command can't read site.db; cmd/logs.go).
				link, citeHere, citeThere, err := cmd.SisterRule(st.Site(), groupID, m.Group)
				if err != nil {
					return nil, err
				}
				if o != nil && link {
					res.Sisters = append(res.Sisters, cmd.SisterMatch{Group: m.Group, Post: o.ID, Version: o.ThreadVersion,
						Title: o.Title, Date: o.CreatedAt, NoteHere: w.IDs.Next(), NoteThere: w.IDs.Next(),
						CiteHere: citeHere, CiteThere: citeThere})
				}
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
			if n.SourcePostID == 0 {
				return w.stamp(res), nil
			}
			if n.SourceGroupID != groupID {
				// A sister-group note: written only while the pairing is
				// active and the visibility rule allows it.
				ok, err := w.sisterAllowed(groupID, n.SourceGroupID)
				if err != nil || !ok {
					return w.stamp(res), err
				}
			}
			text, changed, err = w.Engine.Note(ctx, groupID, n.HostPostID, n.SourceGroupID, n.SourcePostID, n.Text)
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

	case cmd.JobCheckComment:
		cm, err := st.Comment(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		res := &cmd.SetCommentCheck{GroupID: groupID, JobID: j.ID, Worker: w.Name, CommentID: j.RefID}
		if cm == nil || !shown(cm.Status) {
			if cm != nil {
				res.Version = cm.Version
			}
			return w.stamp(res), nil
		}
		res.Version = cm.Version
		p, err := st.Post(groupID, cm.PostID)
		if err != nil || p == nil {
			return nil, err
		}
		v, err := w.Engine.Moderate(ctx, groupID, cm.Body, postText(p))
		if err != nil {
			return nil, err
		}
		res.Verdict, res.Category, res.Reason = v.Verdict, v.Category, v.Reason
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
	case *cmd.SetCommentCheck:
		r.At = at
	}
	return c
}

// Kinds of match candidate.
const (
	kindPost   = "post"
	kindFAQ    = "faq"
	kindPage   = "page"
	kindSister = "sister" // a post in a sister group
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
	sisters, err := w.sisterCandidates(groupID, p, query)
	if err != nil {
		return nil, err
	}
	return append(out, sisters...), nil
}

// sisterCandidates finds posts in sister groups that may be about the same
// thing (plan section 2, "How links are found"): the same full-text
// search, run on each sister group this group may link with, limited to
// the pairing's topics if it has any. This node holds every group (only a
// full copy runs a worker), so it's a local query, not a network call.
func (w *Worker) sisterCandidates(groupID int64, p *store.Post, query string) ([]Candidate, error) {
	st := w.Engine.Store
	sisters, err := st.Sisters(groupID)
	if err != nil || len(sisters) == 0 {
		return nil, err
	}
	have, err := st.SisterLinks(groupID, p.ID)
	if err != nil {
		return nil, err
	}
	linked := map[[2]int64]bool{}
	for _, l := range have {
		linked[[2]int64{l.OtherGroup, l.OtherPost}] = true // active or rejected: don't offer again
	}
	var out []Candidate
	for _, sis := range sisters {
		ok, err := w.sisterAllowed(groupID, sis.Other)
		if err != nil {
			return nil, err
		}
		// Either direction's note may be allowed: this post's note needs the
		// sister to be public, the sister post's note needs this group to be.
		// sisterAllowed covers the first; check the second too.
		back, err := w.sisterAllowed(sis.Other, groupID)
		if err != nil {
			return nil, err
		}
		if !ok && !back {
			continue
		}
		g, err := st.GroupByID(sis.Other)
		if err != nil || g == nil {
			return nil, err
		}
		q := query
		if sis.Topics != "" {
			q = "(" + query + ") AND (" + store.FTSQuery("", strings.Split(sis.Topics, ",")...) + ")"
		}
		hits, err := st.Search(sis.Other, q, 4, false)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if linked[[2]int64{sis.Other, h.PostID}] {
				continue
			}
			o, err := st.Post(sis.Other, h.PostID)
			if err != nil {
				return nil, err
			}
			if o == nil || o.ContinuesID != 0 || o.Status != "visible" {
				continue
			}
			out = append(out, Candidate{ID: o.ID, Kind: kindSister, Group: sis.Other,
				Text: "In the " + g.Name + " group: " + summaryLine(o)})
			if len(out) >= 4 {
				return out, nil
			}
		}
	}
	return out, nil
}

// sisterAllowed: may a note shown in group here be written from a post in
// group from? The pairing must be active, both groups must use AI, and
// the visibility rule (auth.CanCite) must allow it.
func (w *Worker) sisterAllowed(here, from int64) (bool, error) {
	st := w.Engine.Store
	pairs, err := st.Sisters(here)
	if err != nil {
		return false, err
	}
	paired := false
	for _, p := range pairs {
		paired = paired || p.Other == from
	}
	if !paired {
		return false, nil
	}
	h, err := st.GroupByID(here)
	if err != nil || h == nil {
		return false, err
	}
	f, err := st.GroupByID(from)
	if err != nil || f == nil {
		return false, err
	}
	return h.AIEnabled && f.AIEnabled && auth.CanCite(f.Visibility), nil
}

// sweepSisters is the cross-group refresh: for each note written from a
// post in a sister group, if that thread has changed since the note was
// queued, mark the note stale so it's rewritten. (A change in one group
// can't reach into another group's file by itself; see cmd/sisters.go.)
func (w *Worker) sweepSisters(ctx context.Context) error {
	st := w.Engine.Store
	groups, err := st.GroupFileIDs()
	if err != nil {
		return err
	}
	for _, g := range groups {
		notes, err := st.SisterNotes(g)
		if err != nil {
			return err
		}
		for _, n := range notes {
			if ctx.Err() != nil {
				return nil
			}
			o, err := st.Post(n.OtherGroup, n.OtherPost)
			if err != nil {
				return err
			}
			if o == nil || o.ThreadVersion <= n.ExtVersion {
				continue // gone (its deletion already hid the note), or unchanged
			}
			if _, err := w.Log.Apply(&cmd.MarkSisterStale{GroupID: g, NoteID: n.NoteID, Version: o.ThreadVersion,
				Title: o.Title, At: w.Now().Unix()}); err != nil {
				return err
			}
		}
	}
	return nil
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
