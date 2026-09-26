package ai

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/cluster"
)

// Search routing (plan section 9, "How work flows"): a serve node sends
// each search to whichever worker should answer soonest. Workers report
// their state on /ai/health; the serve node polls it every few seconds.

// Health is a worker's report.
type Health struct {
	Applied uint64  `json:"applied"` // its site.db's last stamp (ms), to judge how current its copy is
	Queue   int     `json:"queue"`   // searches in progress
	AvgAsk  float64 `json:"avg_ask"` // recent seconds per search
}

// Handler serves a worker's endpoints on the cluster's internal API.
func (e *Engine) Handler(mux *http.ServeMux, applied func() uint64) {
	mux.HandleFunc("GET /ai/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(e.Health(applied()))
	})
	mux.HandleFunc("POST /ai/ask", func(w http.ResponseWriter, r *http.Request) {
		var req AskRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cards, err := e.Ask(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(cards)
	})
}

// Health is this engine's current report.
func (e *Engine) Health(applied uint64) Health {
	e.mu.Lock()
	avg := e.avgAsk
	e.mu.Unlock()
	if avg == 0 {
		avg = 3 // a guess until the first real search (or bench-llm) says otherwise
	}
	return Health{Applied: applied, Queue: e.gate.load(), AvgAsk: avg}
}

// noteAskTime keeps a rolling average of search time.
func (e *Engine) noteAskTime(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.avgAsk == 0 {
		e.avgAsk = d.Seconds()
		return
	}
	e.avgAsk = 0.8*e.avgAsk + 0.2*d.Seconds()
}

// Pool picks where each search runs: this node's own model, or a worker.
type Pool struct {
	Local *Engine // nil if this node has no model
	// Workers returns the cluster addresses of the other nodes with a
	// model. It's asked on every poll (from the node map), so a worker
	// added or removed is picked up without a restart.
	Workers func() []string
	Client  *cluster.Client
	Applied func() uint64 // this node's site.db's last stamp, in ms (cluster.Node.SiteStamp)

	mu     sync.Mutex
	health map[string]workerState
}

type workerState struct {
	Health
	seen time.Time
}

// Poll keeps the workers' health current until ctx ends.
func (p *Pool) Poll(ctx context.Context) {
	for {
		var workers []string
		if p.Workers != nil {
			workers = p.Workers()
		}
		for _, addr := range workers {
			hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			var h Health
			err := p.Client.GetJSON(hctx, addr, "/ai/health", &h)
			cancel()
			p.mu.Lock()
			if p.health == nil {
				p.health = map[string]workerState{}
			}
			if err == nil {
				p.health[addr] = workerState{h, time.Now()}
			}
			p.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// Available reports whether any search worker is up, for the page to
// decide whether to ask at all.
func (p *Pool) Available() bool {
	_, ok := p.choose()
	return ok
}

// Workers whose copy of site.db is more than this far behind this node's
// (in milliseconds of stamp) are skipped: their answers could miss what
// was just posted. Copies normally trail by a second or two.
const maxLag = 30_000

// choose returns the worker that should finish soonest, estimated as
// (queue + 1) x its average search time. "" means this node.
func (p *Pool) choose() (string, bool) {
	best, bestCost, found := "", 0.0, false
	if p.Local != nil {
		h := p.Local.Health(0)
		best, bestCost, found = "", float64(h.Queue+1)*h.AvgAsk, true
	}
	mine := uint64(0)
	if p.Applied != nil {
		mine = p.Applied()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for addr, s := range p.health {
		if time.Since(s.seen) > 15*time.Second || s.Applied+maxLag < mine {
			continue
		}
		cost := float64(s.Queue+1) * s.AvgAsk
		if !found || cost < bestCost {
			best, bestCost, found = addr, cost, true
		}
	}
	return best, found
}

// Ask runs a search on the best worker. ErrNoWorker means none is up.
func (p *Pool) Ask(ctx context.Context, req AskRequest) ([]Card, error) {
	addr, ok := p.choose()
	if !ok {
		return nil, ErrNoWorker
	}
	if addr == "" {
		return p.Local.Ask(ctx, req)
	}
	var cards []Card
	err := p.Client.PostJSON(ctx, addr, "/ai/ask", req, &cards)
	if err != nil && ctx.Err() == nil {
		log.Printf("ai: search on %s: %v", addr, err)
		p.mu.Lock()
		delete(p.health, addr) // skip it until its next good health report
		p.mu.Unlock()
	}
	return cards, err
}
