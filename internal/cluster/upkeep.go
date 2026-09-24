package cluster

import (
	"errors"
	"os"
	"time"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
)

// upkeep is the loop that keeps this node in step with the node map. It
// runs every tick (and sooner when poked), and each pass:
//
//  1. registers this node in the map, if the map doesn't match its config;
//  2. after a recover, takes the lost voters out of the map (forget);
//  3. starts the logs of groups placed on this node, and stops (and
//     deletes) those taken off it;
//  4. for each log this node leads: brings Raft's membership in line with
//     the map, and relays anything waiting in the log's outbox.
//
// Everything it does is driven by the map, which is itself replicated, so
// every node reaches the same picture without talking to the others about
// it. And every step is safe to repeat, so a pass that fails halfway is
// simply finished by the next one.
func (n *Node) upkeep() {
	defer n.wg.Done()
	tick := time.NewTicker(n.o.tick)
	defer tick.Stop()
	for {
		n.upkeepOnce()
		select {
		case <-n.done:
			return
		case <-tick.C:
		case <-n.kick:
		}
	}
}

func (n *Node) poke() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

func (n *Node) upkeepOnce() {
	if !n.forget() {
		return // recovered, and not yet the site leader to finish it
	}
	n.register()
	n.placeShards()
	n.mu.Lock()
	var leading []*shard
	for _, s := range n.shards {
		if s.isLeader() {
			leading = append(leading, s)
		}
	}
	n.mu.Unlock()
	for _, s := range leading {
		select {
		case <-n.done:
			return
		default:
		}
		n.reconcileMembers(s)
		n.relay(s)
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
			return nd.Addr == n.o.Advertise && nd.Voter == n.o.Voter && nd.Full == n.o.Full
		}
	}
	return false
}

// register submits a RegisterNode when the map doesn't match this node's
// config: on first start, or after the config changed. A new node's copy
// of site.db is empty until it's added to the site log, so it would keep
// seeing itself missing; the pause between tries lets that copy arrive.
func (n *Node) register() {
	if n.registered() || time.Since(n.lastSubmit) < 5*n.o.tick {
		return
	}
	n.lastSubmit = time.Now()
	_, err := n.Apply(&cmd.RegisterNode{ID: n.o.ID, Addr: n.o.Advertise, Voter: n.o.Voter, Full: n.o.Full,
		At: time.Now().Unix()})
	if err != nil {
		logf("registering %s: %v (will retry)", n.o.ID, err)
	}
}

// forget finishes a recover (see Recover): once this node leads the site
// log, it registers itself as a voter and removes every other voter from
// the node map, taking over any group left without a voter. It reports
// whether upkeep can go on: true when there's nothing (left) to do.
//
// Why here and not in Recover: the map lives in site.db, and site.db is
// only ever changed through its log, which needs a running node.
func (n *Node) forget() bool {
	mark := recoveredMark(n.st)
	if _, err := os.Stat(mark); err != nil {
		return true
	}
	s := n.site()
	if !s.isLeader() {
		return false
	}
	now := time.Now().Unix()
	if _, _, err := s.applyHere(&cmd.RegisterNode{ID: n.o.ID, Addr: n.o.Advertise, Voter: true, Full: n.o.Full, At: now}); err != nil {
		logf("recover: %v", err)
		return false
	}
	nodes, err := n.st.Nodes()
	if err != nil {
		return false
	}
	for _, nd := range nodes {
		if nd.ID == n.o.ID || !nd.Voter {
			continue
		}
		if _, _, err := s.applyHere(&cmd.RemoveNode{ID: nd.ID, Replacement: n.o.ID, At: now}); err != nil {
			logf("recover: removing %s: %v", nd.ID, err)
			return false
		}
		logf("recover: removed lost voter %s from the node map", nd.ID)
	}
	os.Remove(mark)
	return true
}

// placeShards starts this node's member of every group log placed on it,
// and stops the ones taken off it, deleting its copy of the group.
func (n *Node) placeShards() {
	n.placeMu.Lock()
	defer n.placeMu.Unlock()
	select {
	case <-n.done:
		return // shutting down: start nothing new
	default:
	}
	mine, err := n.st.HostedBy(n.o.ID)
	if err != nil {
		return
	}
	for gid, h := range mine {
		l := cmd.LogID(gid)
		if n.shard(l) != nil || isEverywhere(l) {
			continue
		}
		var boot []raft.Server
		if h.Voter && h.Bootstrap {
			// One of the group's first voters: start the log with all of
			// them, as each of them is doing too.
			hosts, err := n.st.GroupHosts(gid)
			if err != nil {
				continue
			}
			for _, o := range hosts {
				if o.Voter && o.Bootstrap {
					boot = append(boot, raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(o.NodeID), Address: raft.ServerAddress(o.Addr)})
				}
			}
		}
		s, err := n.openShard(l, boot)
		if err != nil {
			logf("starting %s: %v", l, err)
			continue
		}
		n.mu.Lock()
		n.shards[l] = s
		n.mu.Unlock()
	}

	n.mu.Lock()
	var gone []*shard
	for l, s := range n.shards {
		if _, ok := mine[int64(l)]; !isEverywhere(l) && !ok {
			gone = append(gone, s)
			delete(n.shards, l)
		}
	}
	n.mu.Unlock()
	for _, s := range gone {
		// Taken off this node. If it leads the log, it hands over first:
		// removing itself makes the others elect a new leader, and they
		// don't have to wait out a timeout to notice it's gone.
		if s.isLeader() {
			s.raft.RemoveServer(raft.ServerID(n.o.ID), 0, 10*time.Second).Error()
		}
		s.shutdown()
		os.RemoveAll(s.dir)
		if err := n.st.DropGroup(int64(s.log)); err != nil {
			logf("dropping %s: %v", s.log, err)
		}
		logf("%s taken off this node", s.log)
	}
}

// relay applies what a log's commands sent to other logs (their outbox,
// see internal/cmd/logs.go), then clears what it applied. Only the log's
// leader relays, since only it can clear the outbox; a new leader relays
// whatever its predecessor didn't finish, which may repeat a few, and
// that's why follow-ups are safe to apply twice.
//
// A follow-up that fails because of the person's kind of error (the post
// it was for is gone) is dropped, as it would fail the same way forever.
// Anything else (the target log has no leader right now) stops the relay,
// leaving the rest for the next pass, in order.
func (n *Node) relay(s *shard) {
	if !s.relayMu.TryLock() {
		return // already relaying: that pass picks up anything new too
	}
	defer s.relayMu.Unlock()
	for s.isLeader() {
		items, err := n.st.Outbox(int64(s.log), 100)
		if err != nil || len(items) == 0 {
			return
		}
		for _, it := range items {
			c, err := cmd.Decode(it.Command)
			if err == nil {
				_, err = n.Apply(c)
			}
			if err != nil && !cmd.IsInput(err) {
				logf("relay from %s: %v (will retry)", s.log, err)
				return
			}
		}
		done := &cmd.OutboxDone{GroupID: int64(s.log), UpTo: items[len(items)-1].ID}
		if _, _, err := s.applyHere(done); err != nil {
			if !errors.Is(err, ErrNotLeader) {
				logf("relay from %s: %v", s.log, err)
			}
			return
		}
	}
}
