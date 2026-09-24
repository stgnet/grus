package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/fetch"
	"github.com/stgnet/grus/internal/store"
)

// The FAQ and outside-source jobs (M3).

// faqCluster caps how many threads a new entry is written from.
const faqCluster = 12

func (w *Worker) runFAQ(ctx context.Context, groupID int64, j store.Job) (cmd.Command, error) {
	st := w.Engine.Store
	tree, err := st.Topics(groupID)
	if err != nil {
		return nil, err
	}
	topics := store.FlatTopics(tree)

	switch j.Kind {
	case cmd.JobFAQNew:
		res := &cmd.CreateFAQEntry{GroupID: groupID, JobID: j.ID, Worker: w.Name, EntryID: w.IDs.Next(), Posts: []int64{j.RefID}}
		anchor, err := st.Post(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		if anchor == nil || anchor.Status != "visible" {
			return w.stamp(res), nil // closes the job; nothing to write
		}
		res.Version = anchor.ThreadVersion
		ids, err := st.Cluster(groupID, j.RefID, faqCluster)
		if err != nil {
			return nil, err
		}
		// The anchor stays first: the result's version check is against it.
		res.Posts = []int64{j.RefID}
		var posts []store.Post
		for _, id := range ids {
			p, err := st.Post(groupID, id)
			if err != nil {
				return nil, err
			}
			if p == nil || p.Status != "visible" {
				continue
			}
			posts = append(posts, *p)
			if id != j.RefID {
				res.Posts = append(res.Posts, id)
			}
		}
		pages, err := w.pagesAbout(groupID, posts)
		if err != nil {
			return nil, err
		}
		d, err := w.Engine.FAQNew(ctx, groupID, posts, pages, topics)
		if err != nil {
			return nil, err
		}
		res.Question, res.Answer, res.TopicID, res.Sources = d.Question, d.Answer, d.TopicID, d.Pages
		if d.TopicID == 0 {
			res.NewTopic = &cmd.NewTopic{ID: w.IDs.Next(), ParentID: d.NewParent, Title: d.NewTopic}
		}
		return w.stamp(res), nil

	case cmd.JobFAQRewrite:
		e, err := st.Entry(groupID, j.RefID)
		if err != nil {
			return nil, err
		}
		res := &cmd.SetFAQAnswer{GroupID: groupID, JobID: j.ID, Worker: w.Name, EntryID: j.RefID, NoChange: true}
		if e == nil {
			return nil, errors.New("entry is gone")
		}
		res.Version = e.Version
		if e.Status != "active" {
			return w.stamp(res), nil
		}
		all, err := st.EntryPosts(groupID, e.ID)
		if err != nil {
			return nil, err
		}
		var posts []store.Post
		for _, p := range all {
			// Removed and hidden threads drop out of an entry at its next
			// rewrite: they're not read now, and SetFAQAnswer unlinks them.
			if p.Status == "visible" && len(posts) < faqCluster {
				posts = append(posts, p)
			}
		}
		pages, err := st.SourcesForEntry(groupID, e.ID)
		if err != nil {
			return nil, err
		}
		pages = shownPages(pages)
		comments, err := st.EntryComments(groupID, e.ID)
		if err != nil {
			return nil, err
		}
		if len(posts) == 0 && len(pages) == 0 {
			return w.stamp(res), nil // nothing left to write from; mods decide
		}
		ans, changed, err := w.Engine.FAQRewrite(ctx, e, posts, pages, comments)
		if err != nil {
			return nil, err
		}
		res.Answer, res.NoChange = ans, !changed
		return w.stamp(res), nil

	case cmd.JobOutline:
		res := &cmd.TidyTopics{GroupID: groupID, JobID: j.ID, Worker: w.Name}
		if len(topics) < 4 {
			return w.stamp(res), nil // too few to need tidying
		}
		res.Merges, res.Renames, err = w.Engine.Outline(ctx, topics)
		if err != nil {
			return nil, err
		}
		return w.stamp(res), nil
	}
	return nil, fmt.Errorf("unknown job kind %q", j.Kind)
}

// pagesAbout finds up to three outside pages on the same subject as a set
// of threads, for a new FAQ entry to draw on.
func (w *Worker) pagesAbout(groupID int64, posts []store.Post) ([]store.Source, error) {
	var titles []string
	for _, p := range posts {
		titles = append(titles, p.Title)
	}
	terms := store.Terms(strings.Join(titles, " "))
	if len(terms) > 20 {
		terms = terms[:20]
	}
	hits, err := w.Engine.Store.SearchKind(groupID, "source", store.FTSQuery(strings.Join(terms, " ")), 3)
	if err != nil {
		return nil, err
	}
	var out []store.Source
	for _, h := range hits {
		s, err := w.Engine.Store.Source(groupID, h.ID)
		if err != nil {
			return nil, err
		}
		if s != nil && s.Shown() {
			out = append(out, *s)
		}
	}
	return out, nil
}

func shownPages(in []store.Source) []store.Source {
	var out []store.Source
	for _, s := range in {
		if s.Shown() && s.Status == "active" {
			out = append(out, s)
		}
	}
	return out
}

// runSource reads an outside page (JobSource) or a seed list page
// (JobSeed). Reasons a page can't be read become the source's "problem",
// which mods see; a network hiccup is an error, so the job is retried.
func (w *Worker) runSource(ctx context.Context, groupID int64, j store.Job) (cmd.Command, error) {
	st := w.Engine.Store
	s, err := st.Source(groupID, j.RefID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("source is gone")
	}
	if w.Fetch == nil {
		return nil, errors.New("this node doesn't read outside pages")
	}
	domains, err := st.AllowedDomains(groupID)
	if err != nil {
		return nil, err
	}
	// A member's link is read only from sites the mods allowed. A link a
	// mod added (or seeded, or approved) is allowed by that act, for its
	// own site.
	allowed := func(host string) bool {
		host = strings.TrimPrefix(host, "www.")
		if strings.HasPrefix(host, "port:") {
			// Non-standard ports: never, except in tests, where the page
			// server listens on 127.0.0.1 at a random port.
			return w.Fetch.AllowPrivate
		}
		if s.Via != cmd.ViaMember && (host == s.Site || strings.HasSuffix(host, "."+s.Site)) {
			return true
		}
		for _, d := range domains {
			if host == d || strings.HasSuffix(host, "."+d) {
				return true
			}
		}
		return false
	}
	if s.Via == cmd.ViaMember && s.Status == "active" {
		// Approved by a mod: its own site is fine too.
		base := allowed
		allowed = func(host string) bool {
			h := strings.TrimPrefix(host, "www.")
			return base(host) || h == s.Site || strings.HasSuffix(h, "."+s.Site)
		}
	}

	if j.Kind == cmd.JobSeed {
		res := &cmd.AddSeeds{GroupID: groupID, JobID: j.ID, Worker: w.Name, ListID: s.ID, Version: s.Version}
		page, err := w.Fetch.Get(ctx, s.URL, allowed)
		if err != nil {
			if problem(err) == "" {
				return nil, err
			}
			res.Problem = problem(err)
			return w.stamp(res), nil
		}
		for _, l := range page.Links {
			res.URLs = append(res.URLs, cmd.SeedURL{ID: w.IDs.Next(), URL: l})
			if len(res.URLs) == 50 {
				break
			}
		}
		return w.stamp(res), nil
	}

	res := &cmd.SetSource{GroupID: groupID, JobID: j.ID, Worker: w.Name, SourceID: s.ID, Version: s.Version}
	if s.Via == cmd.ViaDescribed || s.Status == "removed" {
		return w.stamp(res), nil // never read; closes the job
	}
	page, err := w.Fetch.Get(ctx, s.URL, allowed)
	switch {
	case errors.Is(err, fetch.ErrGone):
		res.Gone = true
		return w.stamp(res), nil
	case err != nil && problem(err) != "":
		res.Problem = problem(err)
		return w.stamp(res), nil
	case err != nil:
		return nil, err
	}
	res.ContentHash, res.PublishedAt = page.Hash, page.Published
	if page.Hash == s.Hash && s.Summary != "" {
		return w.stamp(res), nil // unchanged: no need to summarize again
	}
	res.Title, res.Summary, err = w.Engine.SummarizeSource(ctx, s.Site, page.Title, page.Text)
	if err != nil {
		return nil, err
	}
	if res.Summary == "" {
		res.Problem = "the page has nothing to summarize"
		res.ContentHash = ""
	}
	return w.stamp(res), nil
}

// problem turns a fetch error into the reason shown to mods, or "" for an
// error worth retrying (the network, a timeout, a server error).
func problem(err error) string {
	for _, e := range []error{fetch.ErrNotAllowed, fetch.ErrRobots, fetch.ErrNotPage, fetch.ErrPrivate, fetch.ErrTooBig} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return ""
}
