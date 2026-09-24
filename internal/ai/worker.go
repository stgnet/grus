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
		res.Digest, err = w.Engine.Digest(ctx, groupID, j.RefID)
		return w.stamp(res), err

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
		for i, id := range matches {
			if i == 3 {
				break // a post rarely has more than a few true matches; more is noise
			}
			res.Links = append(res.Links, cmd.CheckLink{Other: id, NoteHere: w.IDs.Next(), NoteThere: w.IDs.Next()})
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
		if n == nil || n.SourcePostID == 0 || n.SourceGroupID != groupID {
			return w.stamp(res), nil // removed meanwhile, or not ours to write
		}
		text, changed, err := w.Engine.Note(ctx, groupID, n.HostPostID, n.SourcePostID, n.Text)
		if err != nil {
			return nil, err
		}
		res.Text, res.NoChange = text, !changed
		return w.stamp(res), nil
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
	}
	return c
}

// candidates finds the other posts most likely to be about the same thing,
// with plain full-text search (no model): the words of the post's title and
// opening, ranked by BM25. The model then only has to judge a short list.
func (w *Worker) candidates(groupID int64, p *store.Post) ([]Candidate, error) {
	st := w.Engine.Store
	terms := store.Terms(p.Title + " " + p.Body)
	if len(terms) > 25 {
		terms = terms[:25]
	}
	hits, err := st.Search(groupID, store.FTSQuery(strings.Join(terms, " ")), 12, true)
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
		out = append(out, Candidate{ID: o.ID, Text: summaryLine(o)})
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
