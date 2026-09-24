package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/stgnet/grus/internal/cmd"
)

// shard is this node's member of one log's Raft cluster: site.db's log, or
// one group's. Each has its own Raft log, snapshots and elections, so each
// group can live on its own set of nodes, and a group's leader can be a
// different node from the site's.
type shard struct {
	log   cmd.LogID
	dir   string
	raft  *raft.Raft
	trans *raft.NetworkTransport
	bolt  *raftboltdb.BoltStore

	relayMu sync.Mutex    // one outbox relay at a time (see relay)
	down    chan struct{} // closed by shutdown
	once    sync.Once
}

// shardDir is where one log's Raft files live: raft/site, raft/g42.
func (n *Node) shardDir(l cmd.LogID) string {
	return filepath.Join(n.st.Dir(), "raft", l.String())
}

// raftConfig is the Raft configuration every log uses.
func raftConfig(o Options, l cmd.LogID) *raft.Config {
	out := o.LogOutput
	if out == nil {
		out = os.Stderr
	}
	c := raft.DefaultConfig()
	c.LocalID = raft.ServerID(o.ID)
	c.Logger = hclog.New(&hclog.LoggerOptions{Name: "raft-" + l.String(), Output: out, Level: hclog.Info})
	// Our FSM is the SQLite file itself, which survives a restart, so
	// there's no need to reload the last snapshot into it on every start
	// (for a big group that would be a multi-GB copy each boot). Entries
	// after the snapshot are replayed and the file's applied index skips
	// what it already has.
	c.NoSnapshotRestoreOnStart = true
	if o.tune != nil {
		o.tune(c)
	}
	return c
}

// openStores opens one log's bolt file (its entries and Raft's small bit of
// state: term, vote) and snapshot directory.
func openStores(dir string, logger hclog.Logger) (*raftboltdb.BoltStore, *raft.FileSnapshotStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, err
	}
	bolt, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft.db"))
	if err != nil {
		return nil, nil, err
	}
	// Keep two snapshots: the latest, and one before it in case the latest
	// was written badly.
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, 2, logger)
	if err != nil {
		bolt.Close()
		return nil, nil, err
	}
	return bolt, snaps, nil
}

// openShard starts this node's member of log l. With boot set, and no
// state on disk yet, it creates the log with those voters: every one of
// them does the same with the same list, which is how Raft expects a new
// cluster to start. Without it, the node waits for the log's leader to add
// it.
func (n *Node) openShard(l cmd.LogID, boot []raft.Server) (*shard, error) {
	c := raftConfig(n.o, l)
	dir := n.shardDir(l)
	bolt, snaps, err := openStores(dir, c.Logger)
	if err != nil {
		return nil, err
	}
	stream := newTLSStream(n.mux, l, n.o.Advertise, n.o.TLS)
	trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  stream,
		MaxPool: 3,
		Timeout: 10 * time.Second,
		Logger:  c.Logger,
	})
	r, err := raft.NewRaft(c, &fsm{st: n.st, log: l}, bolt, bolt, snaps, trans)
	if err != nil {
		trans.Close()
		bolt.Close()
		return nil, err
	}
	s := &shard{log: l, dir: dir, raft: r, trans: trans, bolt: bolt, down: make(chan struct{})}
	if len(boot) > 0 {
		has, err := raft.HasExistingState(bolt, bolt, snaps)
		if err == nil && !has {
			err = r.BootstrapCluster(raft.Configuration{Servers: boot}).Error()
		}
		if err != nil {
			s.shutdown()
			return nil, err
		}
	}
	return s, nil
}

func (s *shard) shutdown() error {
	s.once.Do(func() { close(s.down) })
	err := s.raft.Shutdown().Error()
	return errors.Join(err, s.trans.Close(), s.bolt.Close())
}

func (s *shard) isLeader() bool { return s.raft.State() == raft.Leader }

func (s *shard) leaderAddr() string {
	addr, _ := s.raft.LeaderWithID()
	return string(addr)
}

// member reports whether this node is in the log's configuration at all:
// a node that's never been added has no one to wait for.
func (s *shard) member() bool {
	f := s.raft.GetConfiguration()
	return f.Error() == nil && len(f.Configuration().Servers) > 0
}

func (s *shard) waitLeader(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.leaderAddr() != "" {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// applyHere appends a command to the log, which works only on the leader.
// It returns the command's log index along with its result.
func (s *shard) applyHere(c cmd.Command) (any, uint64, error) {
	data, err := cmd.Encode(c)
	if err != nil {
		return nil, 0, err
	}
	f := s.raft.Apply(data, 10*time.Second)
	// Wait for the result, or for this log to shut down. Raft's own
	// shutdown can leave a command that was queued at that very moment
	// without an answer forever, and a caller shouldn't hang on that.
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err = <-done:
	case <-s.down:
		return nil, 0, errShutdown
	}
	if err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return nil, 0, ErrNotLeader
		}
		return nil, 0, err
	}
	res := f.Response().(result)
	return res.value, f.Index(), res.err
}

// errShutdown is the answer to a command caught by this node shutting down.
var errShutdown = errors.New("this node is shutting down")

// waitApplied waits until this node has applied the log up to index.
func (s *shard) waitApplied(index uint64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for s.raft.AppliedIndex() < index && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// reconcileMembers makes the log's Raft membership match the node map:
// adds the nodes that should hold it, promotes or demotes voters, and
// removes nodes that shouldn't be there. Only the leader can change
// membership, so only the leader calls this.
//
// A new voter is added as a non-voter first and promoted once it has
// caught up. Adding it as a voter straight away would raise the quorum
// before it had anything, and writes to the log would stall until it
// finished copying the whole group.
func (n *Node) reconcileMembers(s *shard) {
	want, err := n.wantMembers(s.log)
	if err != nil || len(want) == 0 {
		return
	}
	self := raft.ServerID(n.o.ID)
	if _, ok := want[self]; !ok {
		// This node isn't in the map for this log: before it registers
		// (the site log), or while it's being taken off a group. Neither
		// is a time to rearrange everyone else.
		return
	}
	f := s.raft.GetConfiguration()
	if f.Error() != nil {
		return
	}
	have := map[raft.ServerID]raft.Server{}
	for _, sv := range f.Configuration().Servers {
		have[sv.ID] = sv
	}
	const wait = 10 * time.Second
	for id, w := range want {
		if id == self {
			continue
		}
		h, ok := have[id]
		switch {
		case !ok || h.Address != w.Address && h.Suffrage != raft.Voter:
			// New, or moved: in as a non-voter (promoted below, later).
			s.raft.AddNonvoter(id, w.Address, 0, wait)
		case h.Address != w.Address:
			s.raft.AddVoter(id, w.Address, 0, wait) // updates a voter's address
		case w.Suffrage == raft.Voter && h.Suffrage != raft.Voter:
			if n.caughtUp(s, string(w.Address)) {
				s.raft.AddVoter(id, w.Address, 0, wait)
			}
		case w.Suffrage != raft.Voter && h.Suffrage == raft.Voter:
			s.raft.DemoteVoter(id, 0, wait)
		}
	}
	for id := range have {
		if _, ok := want[id]; !ok && id != self {
			s.raft.RemoveServer(id, 0, wait)
		}
	}
}

// wantMembers is who the node map says should be in a log: every node for
// the logs every node holds (voters by their voter flag), a group's hosts
// for a group's.
// It returns nothing when the map has no voter for the log, which is only
// true before the first node has registered, and not a membership to make.
func (n *Node) wantMembers(l cmd.LogID) (map[raft.ServerID]raft.Server, error) {
	want := map[raft.ServerID]raft.Server{}
	voters := 0
	add := func(id, addr string, voter bool) {
		sf := raft.Nonvoter
		if voter {
			sf = raft.Voter
			voters++
		}
		want[raft.ServerID(id)] = raft.Server{ID: raft.ServerID(id), Address: raft.ServerAddress(addr), Suffrage: sf}
	}
	if isEverywhere(l) {
		nodes, err := n.st.Nodes()
		if err != nil {
			return nil, err
		}
		for _, nd := range nodes {
			add(nd.ID, nd.Addr, nd.Voter)
		}
	} else {
		hosts, err := n.st.GroupHosts(int64(l))
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			add(h.NodeID, h.Addr, h.Voter)
		}
	}
	if voters == 0 {
		return nil, nil
	}
	return want, nil
}

// caughtUp asks a node how far it has applied a log, and says whether
// that's close enough to the leader's end of the log to give it a vote.
func (n *Node) caughtUp(s *shard, addr string) bool {
	applied, err := n.client.Applied(addr, s.log)
	if err != nil {
		return false
	}
	return applied+catchUpSlack >= s.raft.LastIndex()
}

// catchUpSlack is how many entries behind a node may be when it's promoted
// to voter: a few seconds of writes, which it copies in well under a second.
const catchUpSlack = 64
