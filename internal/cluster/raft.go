package cluster

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
	"github.com/stgnet/grus/internal/store"
)

// Options configure a Raft node.
type Options struct {
	ID        string        // stable Raft id: "n1", "studio"
	Listen    string        // cluster port to listen on, e.g. ":7946"
	Advertise string        // host:port other nodes dial to reach this one
	TLS       *tls.Config   // from LoadTLS
	Bootstrap bool          // create a new cluster with this node as its only voter
	Peers     []config.Peer // non-voters the leader keeps in the cluster
	LogOutput io.Writer     // where Raft's own log lines go (default stderr)

	tune func(*raft.Config) // tests shorten the timeouts
}

// Node is this server's member of the Raft cluster.
type Node struct {
	raft   *raft.Raft
	trans  *raft.NetworkTransport
	boltDB *raftboltdb.BoltStore
	peers  []config.Peer
	done   chan struct{}
	rpc    *subListener
}

// RPCListener is where the internal HTTP API's connections arrive (see
// mux.go). The caller serves it with RPCHandler.
func (n *Node) RPCListener() net.Listener { return n.rpc }

// LeaderAddr is the cluster address of the current leader, or "" if there
// is none right now.
func (n *Node) LeaderAddr() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// raftDir is where Raft keeps its log and snapshots, next to the databases.
func raftDir(st *store.Store) string { return filepath.Join(st.Dir(), "raft") }

// open builds the pieces NewRaft and RecoverCluster both need.
func open(o Options, st *store.Store) (*raft.Config, *raftboltdb.BoltStore, *raft.FileSnapshotStore, *raft.NetworkTransport, *muxListener, error) {
	dir := raftDir(st)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	out := o.LogOutput
	if out == nil {
		out = os.Stderr
	}
	logger := hclog.New(&hclog.LoggerOptions{Name: "raft", Output: out, Level: hclog.Info})

	c := raft.DefaultConfig()
	c.LocalID = raft.ServerID(o.ID)
	c.Logger = logger
	// Our FSM is the SQLite files themselves, which survive a restart, so
	// there's no need to reload the last snapshot into them on every start
	// (for a big group that would be a multi-GB copy each boot). Entries
	// after the snapshot are replayed and the files' applied indexes skip
	// what they already have.
	c.NoSnapshotRestoreOnStart = true
	if o.tune != nil {
		o.tune(c)
	}

	// One bolt file holds both the log entries and Raft's small amount of
	// state (current term, vote).
	bolt, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft.db"))
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	// Keep two snapshots: the latest, and one before it in case the latest
	// was written badly.
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, 2, logger)
	if err != nil {
		bolt.Close()
		return nil, nil, nil, nil, nil, err
	}
	mux, err := newMux(o.Listen, o.TLS)
	if err != nil {
		bolt.Close()
		return nil, nil, nil, nil, nil, err
	}
	stream := newTLSStream(mux, o.Advertise, o.TLS)
	trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  stream,
		MaxPool: 3,
		Timeout: 10 * time.Second,
		Logger:  logger,
	})
	return c, bolt, snaps, trans, mux, nil
}

// Start joins (or, with Bootstrap, creates) the cluster and starts applying
// the log to st.
func Start(o Options, st *store.Store) (*Node, error) {
	c, bolt, snaps, trans, mux, err := open(o, st)
	if err != nil {
		return nil, err
	}
	r, err := raft.NewRaft(c, &fsm{st: st}, bolt, bolt, snaps, trans)
	if err != nil {
		trans.Close()
		bolt.Close()
		return nil, err
	}
	n := &Node{raft: r, trans: trans, boltDB: bolt, peers: o.Peers, done: make(chan struct{}),
		rpc: &subListener{m: mux, ch: mux.rpc, addr: hostAddr(o.Advertise)}}

	if o.Bootstrap {
		// Only the very first start creates the cluster; after that the
		// flag is harmless, which keeps the config file unchanged.
		has, err := raft.HasExistingState(bolt, bolt, snaps)
		if err != nil {
			n.Shutdown()
			return nil, err
		}
		if !has {
			f := r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
				Suffrage: raft.Voter, ID: raft.ServerID(o.ID), Address: raft.ServerAddress(o.Advertise),
			}}})
			if err := f.Error(); err != nil {
				n.Shutdown()
				return nil, err
			}
		}
	}
	go n.keepPeers()
	return n, nil
}

// keepPeers adds the configured non-voters (the Studio) to the cluster
// whenever this node becomes leader. Declaring them in the leader's config
// file, rather than having a join step, means re-adding a rebuilt Studio is
// just restarting it.
func (n *Node) keepPeers() {
	for {
		select {
		case <-n.done:
			return
		case leader := <-n.raft.LeaderCh():
			if !leader {
				continue
			}
			f := n.raft.GetConfiguration()
			if err := f.Error(); err != nil {
				continue
			}
			have := map[raft.ServerID]raft.ServerAddress{}
			for _, s := range f.Configuration().Servers {
				have[s.ID] = s.Address
			}
			for _, p := range n.peers {
				if addr, ok := have[raft.ServerID(p.ID)]; ok && addr == raft.ServerAddress(p.Addr) {
					continue
				}
				// AddNonvoter also updates the address of a known peer.
				n.raft.AddNonvoter(raft.ServerID(p.ID), raft.ServerAddress(p.Addr), 0, 10*time.Second)
			}
		}
	}
}

// ErrNotLeader means this node can't accept writes right now. In M0 only
// the VPS serves web traffic and it's the only voter, so it's always the
// leader once it has started; forwarding writes from other nodes comes with
// multiple VPS nodes (M7).
var ErrNotLeader = errors.New("this node is not the cluster leader")

// Apply submits a command and waits until it's committed and applied here.
func (n *Node) Apply(c cmd.Command) (any, error) {
	data, err := cmd.Encode(c)
	if err != nil {
		return nil, err
	}
	f := n.raft.Apply(data, 10*time.Second)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return nil, ErrNotLeader
		}
		return nil, err
	}
	res := f.Response().(result)
	return res.value, res.err
}

// IsLeader reports whether this node is the leader right now.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// WaitLeader waits until some node is leader (this one or another), for
// startup and tests.
func (n *Node) WaitLeader(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if addr, _ := n.raft.LeaderWithID(); addr != "" {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no cluster leader after %s", timeout)
}

// Barrier waits until everything committed so far is applied on this node.
func (n *Node) Barrier(timeout time.Duration) error { return n.raft.Barrier(timeout).Error() }

// AppliedIndex is the last log index applied on this node.
func (n *Node) AppliedIndex() uint64 { return n.raft.AppliedIndex() }

// Stats is Raft's status map, for the admin page.
func (n *Node) Stats() map[string]string { return n.raft.Stats() }

// Shutdown leaves the cluster running without this node.
func (n *Node) Shutdown() error {
	close(n.done)
	err := n.raft.Shutdown().Error()
	return errors.Join(err, n.trans.Close(), n.boltDB.Close())
}

// Recover rewrites this node's copy of the cluster membership so it's the
// cluster's only voter, keeping all its data and log. It's the "the VPS is
// gone" runbook step: run it (with the server stopped) on a copy of the
// Studio's data directory under the new node's id, then start the server
// normally. See docs/operations.md.
func Recover(o Options, st *store.Store) error {
	c, bolt, snaps, trans, _, err := open(o, st)
	if err != nil {
		return err
	}
	defer bolt.Close()
	defer trans.Close()
	conf := raft.Configuration{Servers: []raft.Server{{
		Suffrage: raft.Voter, ID: raft.ServerID(o.ID), Address: raft.ServerAddress(o.Advertise),
	}}}
	return raft.RecoverCluster(c, &fsm{st: st}, bolt, bolt, snaps, trans, conf)
}
