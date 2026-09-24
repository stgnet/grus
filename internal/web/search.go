package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/ai"
	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Search and Ask (plan section 3). One box. The page shows plain full-text
// results at once, each with its thread's digest; for a signed-in reader in
// a group that uses AI, a moment later the page asks for fact cards (/ask)
// and shows them above. If no worker is up, or the cards take longer than
// askTimeout, the plain results stay with one plain line above them. Search
// never shows an error and never leaves a spinner.

// askTimeout is how long search waits for fact cards (plan section 3). A
// variable only so tests can shorten it.
var askTimeout = 8 * time.Second

const (
	defaultAskLimit = 20 // questions per person per day
	softFailText    = "Quick answers aren't available right now. These posts match your search."
)

type searchHit struct {
	ID       int64
	Title    string
	Date     int64
	Comments int
	About    string // the digest, or the passage that matched
}

type searchData struct {
	Q, Prev string
	FAQ     []store.Entry  // matching FAQ entries: the group's best summaries, shown first
	Hits    []searchHit    // threads
	Pages   []store.Source // outside pages, labeled with their site
	Ask     bool           // the page should ask for fact cards
	Related string
}

// searchPage is /search?q=: plain results, plus a place for fact cards.
func (s *Server) searchPage(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	if !c.canRead(nil) {
		s.render(w, r, http.StatusOK, "group", c.page(c.g.Name, feedData{Settings: c.st}))
		return
	}
	d := searchData{Q: strings.TrimSpace(r.URL.Query().Get("q")), Prev: strings.TrimSpace(r.URL.Query().Get("prev"))}
	if len(d.Q) > 300 {
		d.Q = d.Q[:300]
	}
	if d.Q != "" {
		var err error
		if d.Hits, err = s.plainSearch(c, d.Prev+" "+d.Q, 20); err != nil {
			s.serverError(w, r, err)
			return
		}
		if d.FAQ, d.Pages, err = s.otherHits(c, d.Prev+" "+d.Q); err != nil {
			s.serverError(w, r, err)
			return
		}
		var ids []string
		for i, h := range d.Hits {
			if i == 3 {
				break
			}
			ids = append(ids, strconv.FormatInt(h.ID, 10))
		}
		d.Related = strings.Join(ids, ",")
		// Fact cards need a login: that keeps anonymous scrapers from
		// running the model (plan section 3).
		d.Ask = c.u != nil && c.st.AIEnabled && s.AI != nil
	}
	title := "Search"
	if d.Q != "" {
		title = d.Q + " · Search"
	}
	s.render(w, r, http.StatusOK, "search", c.page(title, d))
}

// plainSearch is full-text search limited to what this reader may see.
func (s *Server) plainSearch(c *greq, q string, limit int) ([]searchHit, error) {
	hits, err := s.Store.Search(c.g.ID, store.FTSQuery(q), limit, true)
	if err != nil {
		return nil, err
	}
	var out []searchHit
	for _, h := range hits {
		p, err := s.Store.Post(c.g.ID, h.PostID)
		if err != nil {
			return nil, err
		}
		if p == nil || !c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) {
			continue
		}
		about := p.Digest
		if about == "" {
			about = strings.NewReplacer("[", "", "]", "").Replace(h.Snippet)
		}
		out = append(out, searchHit{ID: p.ID, Title: p.Title, Date: p.CreatedAt, Comments: p.CommentCount, About: about})
	}
	return out, nil
}

// otherHits is plain search over the FAQ and outside pages.
func (s *Server) otherHits(c *greq, q string) ([]store.Entry, []store.Source, error) {
	query := store.FTSQuery(q)
	var entries []store.Entry
	hits, err := s.Store.SearchKind(c.g.ID, "faq", query, 3)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range hits {
		if e, err := s.Store.Entry(c.g.ID, h.ID); err == nil && e != nil && e.Status == "active" {
			entries = append(entries, *e)
		}
	}
	var pages []store.Source
	if hits, err = s.Store.SearchKind(c.g.ID, "source", query, 5); err != nil {
		return nil, nil, err
	}
	for _, h := range hits {
		if src, err := s.Store.Source(c.g.ID, h.ID); err == nil && src != nil && src.Shown() {
			pages = append(pages, *src)
		}
	}
	return entries, pages, nil
}

// cardView is one fact card: what a thread, a FAQ entry, or an outside
// page says about the question.
type cardView struct {
	URL       string
	Title     string
	Label     string // "FAQ", or an outside page's site
	External  bool
	Date      int64
	Statement string
}

type cardsData struct {
	Q, Prev  string
	Cards    []cardView
	Soft     string // the one plain line when there are no cards
	CitedIDs string
}

// ask returns the fact cards for a search, as a piece of HTML the search
// page puts in place. Every failure becomes the soft-fail line.
func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	prev := strings.TrimSpace(r.FormValue("prev"))
	d := cardsData{Q: q, Prev: prev}
	switch {
	case c.u == nil || !c.canRead(nil) || !c.st.AIEnabled || s.AI == nil || q == "" || len(q) > 300:
		d.Soft = softFailText
	case !s.askAllowed(c.u.ID):
		// Past the daily limit, search soft-fails like any other time the
		// model can't answer (plan section 3, "Cost controls").
		d.Soft = softFailText
		s.Meter.Count("ask_soft_fail_limit")
	default:
		s.Meter.Count("search")
		ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
		cards, err := s.AI.Ask(ctx, ai.AskRequest{GroupIDs: []int64{c.g.ID}, Question: q, Prev: prev})
		cancel()
		switch {
		case errors.Is(err, ai.ErrNoWorker):
			s.Meter.Count("ask_soft_fail_no_worker")
			d.Soft = softFailText
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.Meter.Count("ask_soft_fail_timeout")
			d.Soft = softFailText
		case err != nil:
			log.Printf("ask: %v", err)
			s.Meter.Count("ask_soft_fail_error")
			d.Soft = softFailText
		default:
			var ids []string
			for _, card := range cards {
				// The worker was told which groups to search; the cards are
				// checked again here against this reader, on the way out
				// (plan section 3, privacy rule 2).
				if card.GroupID != c.g.ID {
					continue
				}
				v, ok := s.checkCard(c, card)
				if !ok {
					continue
				}
				d.Cards = append(d.Cards, v)
				ids = append(ids, strconv.FormatInt(card.PostID+card.EntryID+card.SourceID, 10))
			}
			d.CitedIDs = strings.Join(ids, ",")
		}
	}
	s.renderFragment(w, r, "cards", d)
}

// checkCard re-checks one card against this reader and what's shown now,
// and fills in what the page needs to show it.
func (s *Server) checkCard(c *greq, card ai.Card) (cardView, bool) {
	switch {
	case card.EntryID != 0:
		e, err := s.Store.Entry(c.g.ID, card.EntryID)
		if err != nil || e == nil || e.Status != "active" {
			return cardView{}, false
		}
		return cardView{URL: fmt.Sprintf("/faq/e/%d", e.ID), Title: e.Question, Label: "FAQ", Date: e.UpdatedAt, Statement: card.Statement}, true
	case card.SourceID != 0:
		src, err := s.Store.Source(c.g.ID, card.SourceID)
		if err != nil || src == nil || !src.Shown() || src.Status != "active" {
			return cardView{}, false
		}
		date := src.PublishedAt
		if date == 0 {
			date = src.CreatedAt
		}
		return cardView{URL: src.URL, Title: src.Title, Label: src.Site, External: true, Date: date, Statement: card.Statement}, true
	}
	p, err := s.Store.Post(c.g.ID, card.PostID)
	if err != nil || p == nil || p.Status != "visible" || !c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) {
		return cardView{}, false
	}
	return cardView{URL: fmt.Sprintf("/p/%d", p.ID), Title: p.Title, Date: p.CreatedAt, Statement: card.Statement}, true
}

// askFeedback saves a search someone marked "not what I was looking for".
func (s *Server) askFeedback(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	if c.u == nil || !c.canRead(nil) {
		s.notFound(w, r)
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	cited := strings.TrimSpace(r.FormValue("cited"))
	if len(q) > 300 || len(cited) > 500 {
		http.Error(w, "too long", http.StatusBadRequest)
		return
	}
	if _, err := s.Log.Apply(&cmd.SaveFeedback{GroupID: c.g.ID, ID: s.IDs.Next(), UserID: c.u.ID, Question: q,
		CitedIDs: cited, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderFragment(w, r, "feedback-saved", nil)
}

type similarData struct {
	Hits []searchHit
}

// similar is the "these earlier posts may answer this" list under the
// title while writing a post: plain full-text matching, so it's instant.
// Each has a "Link to this" box, which links the new post to it.
func (s *Server) similar(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var d similarData
	if c.canRead(nil) && len(store.Terms(q)) > 0 && len(q) <= 300 {
		var err error
		if d.Hits, err = s.plainSearch(c, q, 5); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.renderFragment(w, r, "similar", d)
}

// askCounter enforces the per-person daily limit on questions. It's kept in
// memory on the node that serves the page: that's one node until M7, and
// even then a person's requests mostly land on one node, so the limit holds
// closely enough for what it's for (protecting the model's capacity).
type askCounter struct {
	mu    sync.Mutex
	day   string
	count map[int64]int
}

func (s *Server) askAllowed(userID int64) bool {
	limit := s.AskLimit
	if limit == 0 {
		limit = defaultAskLimit
	}
	today := s.Now().UTC().Format("2006-01-02")
	s.asks.mu.Lock()
	defer s.asks.mu.Unlock()
	if s.asks.day != today {
		s.asks.day, s.asks.count = today, map[int64]int{}
	}
	if s.asks.count[userID] >= limit {
		return false
	}
	s.asks.count[userID]++
	return true
}

// renderFragment writes one piece of HTML (no page layout) for the page's
// script to put in place.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := s.fragments.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render fragment %s: %v", name, err)
	}
}

// relatedIDs parses "1,2,3" (the "Post this question" link's closest
// threads) into ids.
func relatedIDs(s string) []int64 {
	var out []int64
	for _, f := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil && id > 0 && len(out) < 5 {
			out = append(out, id)
		}
	}
	return out
}
