package ai

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/store"
)

// Ask is the question-answering search (plan section 3, "How it works"):
// a fixed pipeline of two short model calls around an ordinary search,
// because a small model is far more reliable at one well-defined step than
// at deciding for itself what to do next.
//
//  1. Expand: the question becomes a few search phrases (synonyms, the
//     group's jargon).
//  2. Search: plain full-text search over the groups the asker can read,
//     plus the threads the best hits are linked to, which lets an answer
//     follow a chain of threads across years.
//  3. Pick: the model reads each thread's stored digest and returns ranked
//     cards: a thread and what it says about the question.

// AskRequest is one search. GroupIDs are the groups the asker may read,
// worked out by the serve node from their session; the worker searches
// those and nothing else.
type AskRequest struct {
	GroupIDs []int64 `json:"groups"`
	Question string  `json:"q"`
	Prev     string  `json:"prev,omitempty"` // the search this one refines
}

const (
	askThreads    = 15 // threads from the search step
	askWithLinked = 20 // plus threads they link to, up to this many
)

// Ask runs the pipeline on this node.
func (e *Engine) Ask(ctx context.Context, req AskRequest) ([]Card, error) {
	e.gate.searchStart()
	defer e.gate.searchDone()
	start := time.Now()
	defer func() { e.noteAskTime(time.Since(start)) }()

	// The serve node sent only groups the asker can read; a group with
	// "Use AI in this group" off is left out here as well.
	var groups []int64
	for _, g := range req.GroupIDs {
		if st, err := e.Store.GroupSettings(g); err == nil && st != nil && st.AIEnabled {
			groups = append(groups, g)
		}
	}
	req.GroupIDs = groups

	phrases, err := e.Expand(ctx, req.Question, req.Prev)
	if err != nil {
		return nil, err
	}
	query := store.FTSQuery(req.Prev+" "+req.Question, phrases...)

	type found struct {
		group int64
		hit   store.Hit
	}
	var all []found
	for _, g := range req.GroupIDs {
		hits, err := e.Store.Search(g, query, askThreads, false)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			all = append(all, found{g, h})
		}
	}
	// BM25 scores from different groups aren't strictly comparable, but
	// they're close enough to merge a group with its sisters.
	sort.SliceStable(all, func(i, j int) bool { return all[i].hit.Rank < all[j].hit.Rank })
	if len(all) > askThreads {
		all = all[:askThreads]
	}

	type key struct{ g, p int64 }
	seen := map[key]bool{}
	var threads []ThreadSummary
	// FAQ entries go first: an entry is the best summary a group has, so
	// Ask looks there before the threads (plan section 4, "Ask and the FAQ
	// work together"). Then the threads, then a few outside pages.
	for _, g := range req.GroupIDs {
		hits, err := e.Store.SearchKind(g, "faq", query, 3)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			en, err := e.Store.Entry(g, h.ID)
			if err != nil {
				return nil, err
			}
			if en != nil && en.Status == "active" {
				threads = append(threads, ThreadSummary{GroupID: g, EntryID: en.ID, Title: en.Question, Date: en.UpdatedAt, Digest: en.Answer})
			}
		}
	}
	add := func(g, id int64) error {
		if seen[key{g, id}] || len(threads) >= askWithLinked {
			return nil
		}
		seen[key{g, id}] = true
		p, err := e.Store.Post(g, id)
		if err != nil || p == nil || p.Status != "visible" {
			return err // flagged content isn't cited until a vote clears it
		}
		threads = append(threads, ThreadSummary{GroupID: g, PostID: p.ID, Title: p.Title, Date: p.CreatedAt, Digest: digestOrOpening(p)})
		return nil
	}
	for _, f := range all {
		if err := add(f.group, f.hit.PostID); err != nil {
			return nil, err
		}
	}
	for i, f := range all {
		if i == 5 {
			break
		}
		linked, err := e.Store.LinkedPosts(f.group, f.hit.PostID)
		if err != nil {
			return nil, err
		}
		for _, id := range linked {
			if err := add(f.group, id); err != nil {
				return nil, err
			}
		}
	}
	for _, g := range req.GroupIDs {
		hits, err := e.Store.SearchKind(g, "source", query, 3)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			src, err := e.Store.Source(g, h.ID)
			if err != nil {
				return nil, err
			}
			if src != nil && src.Shown() && src.Status == "active" {
				date := src.PublishedAt
				if date == 0 {
					date = src.CreatedAt
				}
				threads = append(threads, ThreadSummary{GroupID: g, SourceID: src.ID, Title: src.Site + ": " + src.Title, Date: date, Digest: src.Summary})
			}
		}
	}
	return e.Pick(ctx, req.Question, threads)
}

// digestOrOpening is what search shows and the pick step reads for a
// thread: its digest, or the start of the post until a digest exists.
func digestOrOpening(p *store.Post) string {
	if p.Digest != "" {
		return p.Digest
	}
	s := strings.Join(strings.Fields(p.Body), " ")
	if r := []rune(s); len(r) > 500 {
		s = string(r[:500]) + "…"
	}
	if s == "" {
		s = "(photo post, no text)"
	}
	return s
}
