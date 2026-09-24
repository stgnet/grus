// Package ids makes the 64-bit ids every row uses.
//
// Why not SQLite's AUTOINCREMENT: every write is a replicated command, and
// the command must carry every value it writes (see internal/cmd). So ids
// are chosen by the node that accepts the request, before the command is
// replicated, and two nodes accepting writes at the same moment must never
// pick the same id.
//
// The layout is the well-known "snowflake" one:
//
//	41 bits  milliseconds since 2026-01-01 UTC   (good for ~69 years)
//	10 bits  node number, 0-1023                 (config: node_num)
//	12 bits  sequence within one millisecond     (4096 ids/ms/node)
//
// Because the time is in the high bits, ids grow with time. The plan relies
// on that: "the smaller id is the older post".
package ids

import (
	"sync"
	"time"
)

// epochMs is 2026-01-01T00:00:00Z in Unix milliseconds.
const epochMs = 1767225600000

const (
	nodeBits = 10
	seqBits  = 12
	maxNode  = 1<<nodeBits - 1
	maxSeq   = 1<<seqBits - 1
)

// Generator hands out ids for one node. It's safe for concurrent use.
type Generator struct {
	mu     sync.Mutex
	node   int64
	lastMs int64
	seq    int64
	now    func() time.Time // replaced in tests
}

// New returns a generator for node number node (0-1023). Values outside that
// range are wrapped into it rather than rejected, so a bad config can't stop
// the server; config.Load is where node numbers are checked.
func New(node int) *Generator {
	return &Generator{node: int64(node) & maxNode, now: time.Now}
}

// Next returns a new id, larger than every id this generator returned before.
func (g *Generator) Next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := g.now().UnixMilli() - epochMs
	// If the clock stepped backwards (NTP correction), keep counting from the
	// last time we used rather than risk repeating an id.
	if ms < g.lastMs {
		ms = g.lastMs
	}
	if ms == g.lastMs {
		g.seq++
		if g.seq > maxSeq {
			// 4096 ids in one millisecond: borrow the next millisecond. The
			// clock catches up almost immediately.
			ms++
			g.seq = 0
		}
	} else {
		g.seq = 0
	}
	g.lastMs = ms
	return ms<<(nodeBits+seqBits) | g.node<<seqBits | g.seq
}
