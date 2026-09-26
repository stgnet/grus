package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// upkeep is the loop that keeps this node in step with the others. It runs
// every tick (and sooner when poked), and each pass:
//
//  1. if this node finds itself removed from the map, comes back as a new
//     node (rejoin);
//  2. registers this node in the map, if the map doesn't match its config;
//  3. stops taking writes if another node has its node number;
//  4. copies the files of groups placed on it, and deletes those taken off;
//  5. hears from every other node (their reports) and pulls what it's
//     missing from them;
//  6. for each file it holds: brings the stable copy up to the stable
//     point, makes the follow-ups the stable copy has sent (if it's on
//     duty for the file), and deletes operations every holder has.
//
// Every step is safe to repeat, so a pass that fails halfway is finished
// by the next one.
func (n *Node) upkeep(ctx context.Context) {
	defer n.wg.Done()
	tick := time.NewTicker(n.o.tick)
	defer tick.Stop()
	for {
		n.upkeepOnce(ctx)
		select {
		case <-n.done:
			return
		case <-tick.C:
		case <-n.kick:
		}
	}
}

func (n *Node) upkeepOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if n.rejoin(ctx) {
		return
	}
	n.findAddress(ctx)
	n.register()
	n.checkNumber()
	n.placeFiles(ctx)
	n.swapReports(ctx)
	n.catchUp(ctx)
	for _, l := range n.heldLogs() {
		if ctx.Err() != nil {
			return
		}
		e := n.engineFor(l)
		if err := n.stabilize(e); err != nil && !errors.Is(err, errShutdown) {
			logf("%s: stable copy: %v", l, err)
		}
		if n.onDutyAmong(n.holderIDs(l)) {
			n.sendFollowUps(e)
		}
		if err := n.prune(e); err != nil && !errors.Is(err, errShutdown) {
			logf("%s: deleting old operations: %v", l, err)
		}
	}
}

// registered reports whether the node map lists this node as its options
// describe.
func (n *Node) registered() bool {
	nodes, err := n.st.Nodes()
	if err != nil {
		return false
	}
	for _, nd := range nodes {
		if nd.ID == n.o.ID {
			return nd.Addr == n.dialable() && nd.Voter == n.o.Voter && nd.Full == n.o.Full && nd.AI == n.o.AI &&
				nd.Origin == n.id.origin() && nd.Num == n.o.Num
		}
	}
	return false
}

// register records this node in the map when the map doesn't match its
// config: on first start, or after the config changed.
func (n *Node) register() {
	if n.registered() || !n.holds(cmd.SiteLog) {
		return
	}
	_, err := n.Apply(&cmd.RegisterNode{ID: n.o.ID, Addr: n.dialable(), Voter: n.o.Voter, Full: n.o.Full, AI: n.o.AI,
		Origin: n.id.origin(), Num: n.o.Num, At: n.o.now().Unix()})
	if err != nil {
		n.warn("register", "registering %s: %v (will retry)", n.o.ID, err)
	}
}

// checkNumber stops this node taking writes while another node in the map
// has its node number: two nodes with one number could make the same id
// for two different things. It takes writes again once that's fixed.
func (n *Node) checkNumber() {
	const prefix = "node number "
	nodes, err := n.st.Nodes()
	if err != nil {
		return
	}
	why := ""
	for _, nd := range nodes {
		if nd.ID != n.o.ID && nd.Num >= 0 && nd.Num == n.o.Num {
			why = fmt.Sprintf(prefix+"%d is also node %s's: give this node another node_num", n.o.Num, nd.ID)
			break
		}
	}
	old := n.refuse.Load().(string)
	if why != old && (why != "" || len(old) >= len(prefix) && old[:len(prefix)] == prefix) {
		n.refuse.Store(why)
		if why != "" {
			logf("not taking writes: %s", why)
		}
	}
}

// placeFiles copies the file of every group placed on this node that it
// hasn't got, and deletes those taken off it (once the other holders have
// everything this node made on them). A group that no other node has a
// copy of is new: its file starts empty here, as it does everywhere.
func (n *Node) placeFiles(ctx context.Context) {
	mine, err := n.st.HostedBy(n.o.ID)
	if err != nil {
		return
	}
	want := []cmd.LogID{cmd.RootGroupID}
	for gid := range mine {
		want = append(want, cmd.LogID(gid))
	}
	for _, l := range want {
		n.mu.Lock()
		have := n.copies[l]
		n.mu.Unlock()
		if have || ctx.Err() != nil {
			continue
		}
		e := n.engineFor(l)
		fresh := true // nobody could be reached who has a copy
		for _, addr := range n.holderAddrs(l) {
			err := n.fetchCopy(ctx, e, addr)
			if err == nil {
				fresh = false
				n.markReady(l)
				break
			}
			if !errors.Is(err, errNoCopy) && !isNetErr(err) {
				fresh = false // it may have one; try again next pass
				logf("copying %s from %s: %v", l, addr, err)
			}
		}
		if fresh {
			e.mu.Lock()
			err := n.prepare(e)
			e.mu.Unlock()
			if err == nil {
				n.markReady(l)
			}
		}
	}

	for _, l := range n.heldAll() {
		if _, placed := mine[int64(l)]; placed || isEverywhere(l) {
			continue
		}
		if !n.othersHaveMine(l) {
			continue // not yet: the other holders still need what this node made
		}
		n.mu.Lock()
		delete(n.copies, l)
		n.mu.Unlock()
		e := n.engineFor(l)
		e.mu.Lock()
		err := n.st.DropGroup(int64(l))
		e.mu.Unlock()
		if err != nil {
			logf("dropping %s: %v", l, err)
			continue
		}
		logf("%s taken off this node", l)
	}
}

// heldAll lists every file this node has a copy of, placed here or not.
func (n *Node) heldAll() []cmd.LogID {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []cmd.LogID
	for l, ok := range n.copies {
		if ok {
			out = append(out, l)
		}
	}
	return out
}

// othersHaveMine reports whether some other node holding a file has every
// operation this node made on it.
func (n *Node) othersHaveMine(l cmd.LogID) bool {
	db, err := n.st.Live(int64(l))
	if err != nil {
		return false
	}
	seqs, err := store.Seqs(db)
	if err != nil {
		return false
	}
	made := seqs[n.id.origin()].Seq
	if made == 0 {
		return true
	}
	for _, id := range n.holderIDs(l) {
		if id == n.o.ID {
			continue
		}
		if r := n.peer(id).report; r != nil && r.Logs[l.String()].Have[n.id.origin()] >= made {
			return true
		}
	}
	return false
}

// sendFollowUps makes the follow-ups a file's stable copy has sent (its
// outbox) into operations on their files, each carrying the identity of
// the operation that sent it, so that one sent twice (by two nodes on
// duty during a split, say) applies once. See docs/replication.md,
// "Follow-ups between files".
func (n *Node) sendFollowUps(e *engine) {
	stable, err := n.st.Stable(e.gid())
	if err != nil {
		return
	}
	items, err := store.FollowUps(stable, 100)
	if err != nil {
		return
	}
	for _, it := range items {
		c, err := cmd.Decode(it.Command)
		if err == nil {
			_, err = n.apply(c, it.Cause)
		}
		if err != nil && !cmd.IsInput(err) && !errors.Is(err, cmd.ErrNotFound) {
			n.warn("followup "+e.log.String(), "follow-up from %s: %v (will retry)", e.log, err)
			return
		}
		// Done (or failed for good, as it would every time): no need to
		// send it again. Removed from both copies here; other nodes that
		// send it too are harmless.
		e.mu.Lock()
		store.FollowUpDone(stable, it.Cause)
		if live, err := n.st.Live(e.gid()); err == nil {
			store.FollowUpDone(live, it.Cause)
		}
		e.mu.Unlock()
	}
}

// rejoin handles finding this node removed from the map (its origin is in
// removed_origins) when it wasn't gone after all: the operations it made
// that the others ignore are made again under a new origin, after its
// files are replaced by fresh copies. Nothing it accepted is lost. It
// reports whether it did anything (the rest of the pass waits for the
// next one).
func (n *Node) rejoin(ctx context.Context) bool {
	removed, err := n.st.RemovedOrigins()
	if err != nil {
		return false
	}
	gone, ok := removed[n.id.origin()]
	if !ok {
		return false
	}
	logf("this node was removed from the map; sending back what the others don't have, as a new origin")
	type saved struct {
		log  cmd.LogID
		data []byte
	}
	var redo []saved
	for _, l := range n.heldAll() {
		db, err := n.st.Live(int64(l))
		if err != nil {
			continue
		}
		rows, err := store.OpsAfter(db, store.Position{}, 0)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.Origin == n.id.origin() && r.Seq > gone[l.String()] {
				redo = append(redo, saved{l, r.Command})
			}
		}
	}
	old := n.id.origin()
	if err := n.id.newIncarnation(n.o.ID, n.o.now()); err != nil {
		logf("rejoin: %v", err)
		return true
	}
	// Fresh copies: installCopy takes in the old live file's operations
	// again, which drops the old origin's ignored ones.
	for _, l := range n.heldAll() {
		for _, addr := range n.holderAddrs(l) {
			if n.fetchCopy(ctx, n.engineFor(l), addr) == nil {
				break
			}
		}
	}
	n.register()
	for _, s := range redo {
		c, err := cmd.Decode(s.data)
		if err != nil {
			continue
		}
		if _, err := n.Apply(c); err != nil {
			logf("rejoin: making %T again: %v", c, err)
		}
	}
	logf("rejoined: %s is now %s; %d operations made again", old, n.id.origin(), len(redo))
	return true
}
