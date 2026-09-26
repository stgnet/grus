// Package cluster keeps every node's copies of the files the same, with no
// leader (docs/replication.md has the design): every write is applied on
// the node that takes it, as an operation, and passed to the other nodes
// holding its file, which apply the same operations in the same order.
//
// Handlers don't see any of that: they submit commands through the Log
// interface (log.go), and read their own node's files.
package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Options configure a node.
type Options struct {
	ID        string      // stable node id: "n1", "studio"
	Num       int         // node number in ids (internal/ids); no two nodes may share one; -1: choose one
	Listen    string      // cluster port to listen on, e.g. ":7946"
	Advertise string      // host:port other nodes dial to reach this one; "" to find it (addr.go)
	TLS       *tls.Config // from LoadTLS

	// Voter: new groups are placed on this node (a VPS that serves
	// pages). Full: it holds every group (the Studio). A node can be
	// neither: a small VPS that holds only the groups placed on it.
	// (The name is from when it meant a vote in Raft.)
	Voter, Full bool
	// AI: this node runs a model (ai_url in its config). It's recorded in
	// the node map, which is how the other nodes find where to send
	// searches.
	AI bool
	// Join lists other nodes' cluster addresses. A new node copies site.db
	// from one of them and registers itself; after that, the node map is
	// how the nodes find each other.
	Join []string

	tick     time.Duration                // how often upkeep runs (default 2s)
	publicIP func(context.Context) string // tests: this node's public IP (default: ask via the domains)
	joinWait time.Duration                // tests: give up joining after this (0 = never)
	ackWait  time.Duration                // how long a write waits for another node to have it (default 500ms)
	now      func() time.Time             // the wall clock (tests replace it)
}

// Node is this server's part of the cluster.
type Node struct {
	o      Options
	st     *store.Store
	id     *identity
	clock  *clock
	mux    *muxListener
	rpc    *subListener
	rpcMux *http.ServeMux
	rpcSrv *http.Server
	client *Client

	mu      sync.Mutex
	engines map[cmd.LogID]*engine
	copies  map[cmd.LogID]bool // files this node has a real copy of (see holds)

	peersMu sync.Mutex
	peers   map[string]peerState

	where   addrState    // where others reach this node (addr.go)
	refuse  atomic.Value // string: why this node won't take writes, "" when it will
	rewinds atomic.Int64 // how many rewinds, for the admin page
	closed  atomic.Bool  // shut down: engines change nothing more

	warnMu sync.Mutex
	warned map[string]time.Time // see warn

	kick   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// everywhere lists the files every node holds, whatever the node map
// says: site.db, which is how nodes know anything at all, and the root
// FAQ's group file (cmd.RootGroupID), which every home page shows.
var everywhere = []cmd.LogID{cmd.SiteLog, cmd.RootGroupID}

func isEverywhere(l cmd.LogID) bool { return slices.Contains(everywhere, l) }

// Start starts this node: its internal API, and the upkeep loop that keeps
// its files in step with the other nodes'. A node with join addresses and
// an empty site.db first copies site.db from one of them (retrying until
// one answers), since it can't do anything useful without it.
func Start(o Options, st *store.Store) (*Node, error) {
	if o.tick == 0 {
		o.tick = 2 * time.Second
	}
	if o.ackWait == 0 {
		o.ackWait = 500 * time.Millisecond
	}
	if o.now == nil {
		o.now = time.Now
	}
	id, err := loadIdentity(st.Dir(), o.ID, o.now())
	if err != nil {
		return nil, err
	}
	mux, err := newMux(o.Listen, o.TLS)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{o: o, st: st, id: id, clock: &clock{last: id.HighWater, now: o.now, id: id},
		mux: mux, client: NewClient(o.TLS), engines: map[cmd.LogID]*engine{}, copies: map[cmd.LogID]bool{},
		peers: map[string]peerState{}, warned: map[string]time.Time{}, kick: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	n.refuse.Store("")
	n.rpc = mux.listen(rpcProto, hostAddr(o.Listen))
	n.serveRPC()

	// Files already on disk are real copies. site.db always exists (the
	// store made it), but an empty one on a node that has somewhere to
	// join isn't: it must be copied first.
	for _, gid := range mustIDs(st.GroupFileIDs()) {
		n.copies[cmd.LogID(gid)] = true
	}
	nodes, _ := st.Nodes()
	if len(nodes) > 0 || len(o.Join) == 0 {
		n.copies[cmd.SiteLog] = true
	} else if err := n.joinCopy(ctx); err != nil {
		n.Shutdown()
		return nil, err
	}

	if n.o.Num < 0 {
		if err := n.chooseNum(); err != nil {
			n.Shutdown()
			return nil, err
		}
	}

	n.wg.Add(1)
	go n.upkeep(ctx)
	return n, nil
}

// chooseNum gives a node with no node_num in its config a number: the one
// it chose before, or the lowest no other node in the map has (site.db has
// just been copied, so the map is current). Two nodes joining at the same
// moment could choose the same; checkNumber catches that, and one of them
// is then given another in its config.
func (n *Node) chooseNum() error {
	if n.id.Num != nil {
		n.o.Num = *n.id.Num
		return nil
	}
	nodes, err := n.st.Nodes()
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for _, nd := range nodes {
		if nd.ID != n.o.ID {
			used[nd.Num] = true
		}
	}
	num := 1
	for used[num] {
		num++
	}
	if num > 1022 {
		return errors.New("no free node number: give this node a node_num in its config")
	}
	n.o.Num = num
	return n.id.setNum(num)
}

// Num is this node's node number, for its id generator.
func (n *Node) Num() int { return n.o.Num }

func mustIDs(ids []int64, err error) []int64 {
	if err != nil {
		return nil
	}
	return ids
}

// joinCopy copies site.db from a join address, trying each in turn until
// one works (or the node is shut down).
func (n *Node) joinCopy(ctx context.Context) error {
	if n.o.joinWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, n.o.joinWait)
		defer cancel()
	}
	for {
		for _, addr := range n.o.Join {
			err := n.fetchCopy(ctx, n.engineFor(cmd.SiteLog), addr)
			if err == nil {
				n.markReady(cmd.SiteLog)
				return nil
			}
			n.warn("join", "copying site.db from %s: %v (will retry)", addr, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.o.tick):
		}
	}
}

func (n *Node) markReady(l cmd.LogID) {
	n.mu.Lock()
	n.copies[l] = true
	n.mu.Unlock()
}

// ID is this node's id.
func (n *Node) ID() string { return n.o.ID }

// holds reports whether this node has a real copy of a file, one it can
// apply writes to: it's placed here and has been copied (or made fresh).
// A group placed here a moment ago isn't held until its copy arrives;
// writes for it go to a node that has one.
func (n *Node) holds(l cmd.LogID) bool {
	n.mu.Lock()
	ok := n.copies[l]
	n.mu.Unlock()
	if !ok {
		return false
	}
	if isEverywhere(l) {
		return true
	}
	hosts, err := n.st.HostedBy(n.o.ID)
	if err != nil {
		return false
	}
	_, placed := hosts[int64(l)]
	return placed
}

// heldLogs lists the files this node holds, in order.
func (n *Node) heldLogs() []cmd.LogID {
	n.mu.Lock()
	var out []cmd.LogID
	for l, ok := range n.copies {
		if ok {
			out = append(out, l)
		}
	}
	n.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return slices.DeleteFunc(out, func(l cmd.LogID) bool { return !n.holds(l) })
}

// Holds reports whether this node holds a group, for the web server:
// whether its pages can be served from this node's own copy.
func (n *Node) Holds(groupID int64) bool {
	return n.holds(cmd.LogID(groupID)) && n.st.HasGroup(groupID)
}

// OnDuty reports whether this node should do the site's once-for-the-
// whole-site work right now (the daily purge, the nightly FAQ batch): it's
// the node with the lowest id among those it has heard from recently. See
// docs/replication.md, "Duties".
func (n *Node) OnDuty() bool {
	nodes, err := n.st.Nodes()
	if err != nil {
		return false
	}
	var ids []string
	for _, nd := range nodes {
		ids = append(ids, nd.ID)
	}
	return n.onDutyAmong(ids)
}

// OnDutyFor reports whether this node should do a group's once-only work
// (its notification email and digest): the lowest-id node heard from
// recently among those holding it, if this node holds it.
func (n *Node) OnDutyFor(groupID int64) bool {
	l := cmd.LogID(groupID)
	return n.holds(l) && n.onDutyAmong(n.holderIDs(l))
}

// SiteStamp is the stamp (in milliseconds) of the last operation applied
// to this node's site.db. The AI pool compares it between nodes to judge
// how current a worker's copy is.
func (n *Node) SiteStamp() uint64 {
	pos, err := store.PositionOf(n.st.Site())
	if err != nil {
		return 0
	}
	return uint64(pos.Stamp >> stampBits)
}

// Apply makes a write: on this node if it holds the command's file, or
// else on a node that does. It returns once the write is stored (and, on
// this node, applied): see docs/replication.md, "Passing operations on".
func (n *Node) Apply(c cmd.Command) (any, error) { return n.apply(c, "") }

// apply is Apply with a cause, for a follow-up (see cmd.Op.Cause).
func (n *Node) apply(c cmd.Command, cause string) (any, error) {
	if why := n.refuse.Load().(string); why != "" {
		return nil, errors.New(why)
	}
	if rm, ok := c.(*cmd.RemoveNode); ok && rm.Origin == "" {
		n.fillRemoval(rm)
	}
	l := cmd.LogOf(c)
	if n.holds(l) {
		v, op, err := n.originate(n.engineFor(l), c, cause)
		if err != nil {
			return nil, err
		}
		n.push(l, op)
		n.poke()
		return v, nil
	}
	if !isEverywhere(l) {
		if g, err := n.st.GroupByID(int64(l)); err == nil && g == nil {
			// No such group at all: a follow-up for a group that's gone,
			// say. That's final, not a reason to retry.
			return nil, fmt.Errorf("group %d: %w", l, cmd.ErrNotFound)
		}
	}
	lastErr := errors.New("no node holding that file can be reached right now")
	for _, addr := range append(n.holderAddrs(l), n.o.Join...) {
		raw, err := n.client.apply(addr, c, cause)
		if errors.Is(err, errNotHeld) || isNetErr(err) {
			lastErr = err
			continue // try the next node
		}
		if err != nil {
			return nil, err
		}
		return decodeValue(raw), nil
	}
	return nil, lastErr
}

// fillRemoval fills in what a RemoveNode needs to say about the node's
// operations (cmd.RemoveNode.Upto): its origin, from the map, and for
// each file the most of its operations that this node or any other has
// (from their reports), so nothing any node already took in is ignored.
func (n *Node) fillRemoval(rm *cmd.RemoveNode) {
	nodes, err := n.st.Nodes()
	if err != nil {
		return
	}
	for _, nd := range nodes {
		if nd.ID == rm.ID {
			rm.Origin = nd.Origin
		}
	}
	if rm.Origin == "" {
		return
	}
	rm.Upto = map[string]int64{}
	for _, l := range n.heldLogs() {
		if db, err := n.st.Live(int64(l)); err == nil {
			if seqs, err := store.Seqs(db); err == nil && seqs[rm.Origin].Seq > 0 {
				rm.Upto[l.String()] = seqs[rm.Origin].Seq
			}
		}
	}
	n.peersMu.Lock()
	for _, p := range n.peers {
		if p.report == nil {
			continue
		}
		for name, lr := range p.report.Logs {
			if seq := lr.Have[rm.Origin]; seq > rm.Upto[name] {
				rm.Upto[name] = seq
			}
		}
	}
	n.peersMu.Unlock()
}

// isNetErr reports whether err is a failure to reach a node, as opposed to
// an answer from one.
func isNetErr(err error) bool {
	var ne net.Error
	var oe *net.OpError
	return errors.As(err, &ne) || errors.As(err, &oe)
}

// Ready waits until this node is in the node map as its options describe,
// and has heard from every other node in it. Tests use it; a server just
// carries on and catches up.
func (n *Node) Ready(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.registered() && n.heardFromAll() {
			return nil
		}
		n.poke()
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("node %s not ready after %s", n.o.ID, timeout)
}

func (n *Node) heardFromAll() bool {
	nodes, err := n.st.Nodes()
	if err != nil {
		return false
	}
	for _, nd := range nodes {
		if nd.ID != n.o.ID && n.peer(nd.ID).report == nil {
			return false
		}
	}
	return true
}

// Stats is a summary for the admin page: this node, and each file it
// holds, with how far it has got.
func (n *Node) Stats() map[string]string {
	st := map[string]string{"node": n.o.ID, "origin": n.id.origin(), "rewinds": fmt.Sprint(n.rewinds.Load())}
	n.where.mu.Lock()
	switch {
	case n.where.dialable != "":
		st["reached at"] = n.where.dialable
	case n.where.publicIP != "":
		st["reached at"] = "nowhere: public IP " + n.where.publicIP + " can't be reached on the cluster port, so this node does the talking"
	default:
		st["reached at"] = "nowhere yet: its public IP isn't known, so this node does the talking"
	}
	n.where.mu.Unlock()
	if why := n.refuse.Load().(string); why != "" {
		st["refusing writes"] = why
	}
	var logs []string
	for _, l := range n.heldLogs() {
		e := n.engineFor(l)
		e.mu.Lock()
		f := n.stablePoint(e)
		var waiting int
		if live, err := n.st.Live(e.gid()); err == nil {
			if stable, err := n.st.Stable(e.gid()); err == nil {
				if spos, err := store.PositionOf(stable); err == nil {
					if rows, err := store.OpsAfter(live, spos, 0); err == nil {
						waiting = len(rows)
					}
				}
			}
		}
		e.mu.Unlock()
		desc := fmt.Sprintf("%s (%d not yet stable", l, waiting)
		if f == 0 {
			desc += ", waiting to hear from every node"
		}
		logs = append(logs, desc+")")
	}
	st["files"] = fmt.Sprint(logs)
	nodes, _ := n.st.Nodes()
	var heard []string
	for _, nd := range nodes {
		if nd.ID == n.o.ID {
			continue
		}
		p := n.peer(nd.ID)
		if p.heard.IsZero() {
			heard = append(heard, nd.ID+": never")
		} else {
			heard = append(heard, fmt.Sprintf("%s: %s ago", nd.ID, time.Since(p.heard).Round(time.Second)))
		}
	}
	st["last heard from"] = fmt.Sprint(heard)
	return st
}

// Shutdown stops this node. The others carry on without it.
func (n *Node) Shutdown() error {
	select {
	case <-n.done:
		return nil // already shut down
	default:
	}
	close(n.done)
	n.cancel()
	// Stop answering other nodes first, including on connections already
	// open: a request arriving mid-shutdown would find files going away.
	if n.rpcSrv != nil {
		n.rpcSrv.Close()
	}
	n.wg.Wait()
	// Wait for anything writing to a file to finish; the engines change
	// nothing after this.
	n.closed.Store(true)
	n.mu.Lock()
	engines := make([]*engine, 0, len(n.engines))
	for _, e := range n.engines {
		engines = append(engines, e)
	}
	n.mu.Unlock()
	for _, e := range engines {
		e.mu.Lock()
		e.mu.Unlock()
	}
	return n.mux.Close()
}

func (n *Node) poke() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

func logf(format string, args ...any) { log.Printf("cluster: "+format, args...) }

// warn logs a problem that upkeep will keep meeting until it's fixed (a
// node that can't be reached, say) at most once a minute per key, so the
// log shows it without filling up.
func (n *Node) warn(key, format string, args ...any) {
	n.warnMu.Lock()
	last := n.warned[key]
	if time.Since(last) < time.Minute {
		n.warnMu.Unlock()
		return
	}
	n.warned[key] = time.Now()
	n.warnMu.Unlock()
	logf(format, args...)
}
