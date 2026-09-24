package ai

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
)

// gate lets one model call run at a time, with searches ahead of
// background jobs: a job only starts when no search is in progress, so a
// person waiting on a search never waits behind a queue of digests (at
// most behind the one job call already running).
type gate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	busy     bool
	searches int // searches in progress (each makes two calls)
}

func (g *gate) init() {
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
}

// enter waits for the model. Background calls also wait while any search
// is in progress.
func (g *gate) enter(ctx context.Context, interactive bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	// sync.Cond can't wait on a context by itself, so wake everyone when
	// this one's context ends; it then sees ctx.Err and gives up.
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()
	for g.busy || (!interactive && g.searches > 0) {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.busy = true
	return nil
}

func (g *gate) leave() {
	g.mu.Lock()
	g.init()
	g.busy = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

// searchStart and searchDone bracket one search, so jobs hold off between
// its two calls.
func (g *gate) searchStart() {
	g.mu.Lock()
	g.searches++
	g.mu.Unlock()
}

func (g *gate) searchDone() {
	g.mu.Lock()
	g.init()
	g.searches--
	g.cond.Broadcast()
	g.mu.Unlock()
}

// load is how many searches are in progress, plus one if the model is busy
// with something: the queue length a serve node weighs when choosing a
// worker.
func (g *gate) load() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.searches
	if g.busy && n == 0 {
		n = 1
	}
	return n
}

// Meter counts this node's AI usage in memory, and reports it to the
// replicated daily totals every few minutes (cmd.RecordUsage), so the
// admin page sees every node's numbers without a write per search.
type Meter struct {
	mu   sync.Mutex
	rows map[string]*cmd.UsageRow
}

// Add counts one call.
func (m *Meter) Add(purpose string, u Usage, failed bool) {
	r := cmd.UsageRow{Purpose: purpose, Calls: 1, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, Seconds: u.Seconds}
	if failed {
		r.Failures = 1
	}
	m.merge(r)
}

// Count adds to a plain counter, like a search soft fail by cause.
func (m *Meter) Count(purpose string) { m.merge(cmd.UsageRow{Purpose: purpose, Calls: 1}) }

func (m *Meter) merge(r cmd.UsageRow) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*cmd.UsageRow{}
	}
	t := m.rows[r.Purpose]
	if t == nil {
		t = &cmd.UsageRow{Purpose: r.Purpose}
		m.rows[r.Purpose] = t
	}
	t.Calls += r.Calls
	t.InputTokens += r.InputTokens
	t.OutputTokens += r.OutputTokens
	t.Seconds += r.Seconds
	t.Failures += r.Failures
}

// take empties the meter, in a fixed order.
func (m *Meter) take() []cmd.UsageRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []cmd.UsageRow
	for _, r := range m.rows {
		out = append(out, *r)
	}
	m.rows = nil
	sort.Slice(out, func(i, j int) bool { return out[i].Purpose < out[j].Purpose })
	return out
}

// Report sends the counts to the log every interval until ctx ends.
func (m *Meter) Report(ctx context.Context, log cluster.Log, node string, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		rows := m.take()
		if len(rows) == 0 {
			continue
		}
		c := &cmd.RecordUsage{Day: time.Now().UTC().Format("2006-01-02"), Node: node, Rows: rows}
		if _, err := log.Apply(c); err != nil {
			for _, r := range rows {
				m.merge(r) // keep them for the next report
			}
		}
	}
}
