package cluster

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Options configure a node.
type Options struct {
	ID        string      // stable node id: "n1", "studio"
	Listen    string      // cluster port to listen on, e.g. ":7946"
	Advertise string      // host:port other nodes dial to reach this one
	TLS       *tls.Config // from LoadTLS

	// Bootstrap creates the cluster, with this node as the site log's
	// only voter. Only the very first node's very first start does
	// anything with it; after that it's harmless, so the config file
	// doesn't have to change.
	Bootstrap bool
	// Voter: this node votes in the site log, and new groups are placed
	// on it as voters (a VPS). Full: it holds every group, as a
	// non-voter (the Studio). A node can be neither: a small VPS that
	// holds only the groups an operator places on it.
	Voter, Full bool
	// AI: this node runs a model (ai_url in its config). It's recorded in
	// the node map, which is how the other nodes find where to send
	// searches, rather than each listing the workers in its own config.
	AI bool
	// Join lists other nodes' cluster addresses. A new node registers
	// itself in the node map through them; after that, the map is how the
	// nodes find each other.
	Join []string

	LogOutput io.Writer // where Raft's own log lines go (default stderr)

	tune func(*raft.Config) // tests shorten the timeouts
	tick time.Duration      // how often upkeep runs (default 2s)
}

// Node is this server's part of the cluster: its member of the site log,
// and of each group log it holds.
type Node struct {
	o      Options
	st     *store.Store
	mux    *muxListener
	rpc    *subListener
	rpcMux *http.ServeMux
	rpcSrv *http.Server
	client *Client

	mu     sync.Mutex
	shards map[cmd.LogID]*shard

	placeMu    sync.Mutex // one placeShards at a time
	lastSubmit time.Time  // when upkeep last submitted a RegisterNode

	kick chan struct{} // runs upkeep now, rather than at the next tick
	done chan struct{}
	wg   sync.WaitGroup
}

// everywhere lists the logs every node holds, whatever the node map says:
// site.db's, which is how nodes know anything at all, and the root FAQ's
// group file (cmd.RootGroupID), which the home page of every node shows.
// Their membership is the node map's list of nodes, voters by their voter
// flag.
var everywhere = []cmd.LogID{cmd.SiteLog, cmd.RootGroupID}

func isEverywhere(l cmd.LogID) bool { return slices.Contains(everywhere, l) }

// Start joins (or, with Bootstrap, creates) the cluster and starts applying
// the logs this node holds to st.
func Start(o Options, st *store.Store) (*Node, error) {
	if o.tick == 0 {
		o.tick = 2 * time.Second
	}
	mux, err := newMux(o.Listen, o.TLS)
	if err != nil {
		return nil, err
	}
	n := &Node{o: o, st: st, mux: mux, client: NewClient(o.TLS), shards: map[cmd.LogID]*shard{},
		kick: make(chan struct{}, 1), done: make(chan struct{})}
	n.rpc = mux.listen(rpcProto, hostAddr(o.Advertise))
	n.serveRPC()

	var boot []raft.Server
	if o.Bootstrap {
		boot = []raft.Server{{Suffrage: raft.Voter, ID: raft.ServerID(o.ID), Address: raft.ServerAddress(o.Advertise)}}
	}
	// The logs every node holds: site.db's, and the root FAQ's file.
	for _, l := range everywhere {
		s, err := n.openShard(l, boot)
		if err != nil {
			n.Shutdown()
			return nil, err
		}
		n.shards[l] = s
	}
	n.wg.Add(1)
	go n.upkeep()
	return n, nil
}

// ID is this node's id.
func (n *Node) ID() string { return n.o.ID }

func (n *Node) shard(l cmd.LogID) *shard {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shards[l]
}

func (n *Node) site() *shard { return n.shard(cmd.SiteLog) }

// LeaderAddr is the cluster address of the site log's leader, or "" if
// there is none right now.
func (n *Node) LeaderAddr() string { return n.site().leaderAddr() }

// IsLeader reports whether this node leads the site log right now. Jobs
// that should run once for the whole site (the daily purge, the nightly
// FAQ batch) run on that node.
func (n *Node) IsLeader() bool { return n.site().isLeader() }

// Leads reports whether this node leads a group's log right now: the node
// that sends that group's notification emails.
func (n *Node) Leads(groupID int64) bool {
	s := n.shard(cmd.LogID(groupID))
	return s != nil && s.isLeader()
}

// Holds reports whether this node holds a group: its log runs here, so
// its file here is a live copy.
func (n *Node) Holds(groupID int64) bool {
	return n.shard(cmd.LogID(groupID)) != nil && n.st.HasGroup(groupID)
}

// WaitLeader waits until the site log has a leader (this node or another),
// for startup and tests.
func (n *Node) WaitLeader(timeout time.Duration) error {
	if !n.site().waitLeader(timeout) {
		return fmt.Errorf("no cluster leader after %s", timeout)
	}
	return nil
}

// Ready waits until this node is in the node map as its options describe,
// and holds every group placed on it. Tests use it; a server just carries
// on and catches up.
func (n *Node) Ready(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.registered() && n.holdsAll() {
			return nil
		}
		n.poke()
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("node %s not ready after %s", n.o.ID, timeout)
}

func (n *Node) holdsAll() bool {
	mine, err := n.st.HostedBy(n.o.ID)
	if err != nil {
		return false
	}
	for gid := range mine {
		s := n.shard(cmd.LogID(gid))
		if s == nil || !s.member() {
			return false
		}
	}
	return true
}

// Barrier waits until everything committed to the site log so far is
// applied on this node.
func (n *Node) Barrier(timeout time.Duration) error { return n.site().raft.Barrier(timeout).Error() }

// AppliedIndex is the last site log index applied on this node. The AI
// pool compares it between nodes to judge whose copy is current.
func (n *Node) AppliedIndex() uint64 { return n.site().raft.AppliedIndex() }

// Stats is the site log's Raft status, plus which logs run here, for the
// admin page.
func (n *Node) Stats() map[string]string {
	st := n.site().raft.Stats()
	n.mu.Lock()
	var logs []string
	for l, s := range n.shards {
		name := l.String()
		if s.isLeader() {
			name += " (leader)"
		}
		logs = append(logs, name)
	}
	n.mu.Unlock()
	sort.Strings(logs)
	st["logs"] = fmt.Sprint(logs)
	st["node"] = n.o.ID
	return st
}

// Shutdown leaves the cluster running without this node.
func (n *Node) Shutdown() error {
	select {
	case <-n.done:
		return nil // already shut down
	default:
	}
	close(n.done)
	// Stop answering other nodes first, including on connections already
	// open: a request arriving mid-shutdown would find the logs going away
	// under it.
	n.rpcSrv.Close()
	n.wg.Wait()
	n.placeMu.Lock() // no placeShards may start a log after this
	defer n.placeMu.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	var errs []error
	for l, s := range n.shards {
		errs = append(errs, s.shutdown())
		delete(n.shards, l)
	}
	errs = append(errs, n.mux.Close())
	return errors.Join(errs...)
}

// ErrNotLeader means there's no leader to take a write right now (an
// election is under way, or this node can't reach the others).
var ErrNotLeader = errors.New("no leader for that log right now")

// Apply submits a command to its log and waits until it's committed, and
// applied here if this node holds that log.
//
// Only a log's leader can append to it. When that's this node, the command
// goes straight in. Otherwise Apply forwards it over the cluster port to the
// leader, or to a node that holds the log (which passes it on), then waits
// until this node has applied it too. That wait is what makes a forwarded
// write read-your-own-write: the page that submitted it reads its own copy
// next, and must see the change.
func (n *Node) Apply(c cmd.Command) (any, error) {
	l := cmd.LogOf(c)
	s := n.shardFor(l)
	if s != nil {
		if s.leaderAddr() == "" && s.member() {
			s.waitLeader(5 * time.Second) // usually a brief gap during an election
		}
		v, _, err := n.applyLocal(s, c)
		if !errors.Is(err, ErrNotLeader) {
			return v, err
		}
	}
	targets, err := n.forwardTargets(l, s)
	if err != nil {
		return nil, err
	}
	lastErr := ErrNotLeader
	for _, addr := range targets {
		raw, index, err := n.client.apply(addr, c)
		if errors.Is(err, ErrNotLeader) || isNetErr(err) {
			lastErr = err
			continue // try the next node
		}
		if s != nil && index > 0 && s.member() {
			s.waitApplied(index, 10*time.Second)
		}
		if err != nil {
			return nil, err
		}
		return decodeValue(raw), nil
	}
	return nil, lastErr
}

// applyLocal applies c on this node, as the leader of its log, then relays
// whatever it sent to other logs straight away, so the whole change is in
// place when the caller carries on. (The upkeep loop retries a relay that
// couldn't finish.)
func (n *Node) applyLocal(s *shard, c cmd.Command) (any, uint64, error) {
	v, index, err := s.applyHere(c)
	if !errors.Is(err, ErrNotLeader) {
		n.relay(s)
	}
	return v, index, err
}

// shardFor is the running shard for log l, starting it first if the node
// map says this node holds it (a group placed a moment ago, before upkeep
// noticed). nil means this node doesn't hold the log.
func (n *Node) shardFor(l cmd.LogID) *shard {
	if s := n.shard(l); s != nil || isEverywhere(l) {
		return s
	}
	hosts, err := n.st.GroupHosts(int64(l))
	if err != nil {
		return nil
	}
	for _, h := range hosts {
		if h.NodeID == n.o.ID {
			n.placeShards()
			return n.shard(l)
		}
	}
	return nil
}

// forwardTargets is where to send a command for log l that this node can't
// apply itself: the log's leader if known, then the nodes that hold it
// (voters first, as they're likelier to lead), then the join addresses.
// Any of them passes it on to the leader.
func (n *Node) forwardTargets(l cmd.LogID, s *shard) ([]string, error) {
	var out []string
	add := func(addr string) {
		if addr != "" && addr != n.o.Advertise && !slices.Contains(out, addr) {
			out = append(out, addr)
		}
	}
	if s != nil {
		add(s.leaderAddr())
	}
	if isEverywhere(l) {
		nodes, _ := n.st.Nodes()
		for _, nd := range nodes {
			if nd.Voter {
				add(nd.Addr)
			}
		}
	} else {
		hosts, err := n.st.GroupHosts(int64(l))
		if err != nil {
			return nil, err
		}
		if len(hosts) == 0 {
			g, err := n.st.GroupByID(int64(l))
			if err == nil && g == nil {
				// No such group at all: a follow-up for a group that's
				// gone, say. That's final, not a reason to retry.
				return nil, fmt.Errorf("group %d: %w", l, cmd.ErrNotFound)
			}
		}
		sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Voter && !hosts[j].Voter })
		for _, h := range hosts {
			add(h.Addr)
		}
	}
	for _, a := range n.o.Join {
		add(a)
	}
	return out, nil
}

// isNetErr reports whether err is a failure to reach a node, as opposed to
// an answer from one.
func isNetErr(err error) bool {
	var ne net.Error
	var oe *net.OpError
	return errors.As(err, &ne) || errors.As(err, &oe)
}

// Recover rewrites this node's copy of every log's membership so that it's
// each log's only voter, keeping all its data and logs. It's the "the VPS
// is gone" runbook step: run it (with the server stopped) on a copy of the
// Studio's data directory under the new node's id, then start the server
// normally. On that start, the node also takes the other voters out of
// the node map (see forget), or the site log's leader would add them
// straight back. See docs/operations.md.
func Recover(o Options, st *store.Store) error {
	ents, err := os.ReadDir(filepath.Join(st.Dir(), "raft"))
	if err != nil {
		return err
	}
	conf := raft.Configuration{Servers: []raft.Server{{
		Suffrage: raft.Voter, ID: raft.ServerID(o.ID), Address: raft.ServerAddress(o.Advertise),
	}}}
	for _, e := range ents {
		l, err := cmd.ParseLogID(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		c := raftConfig(o, l)
		bolt, snaps, err := openStores(filepath.Join(st.Dir(), "raft", e.Name()), c.Logger)
		if err != nil {
			return err
		}
		if has, err := raft.HasExistingState(bolt, bolt, snaps); err != nil || !has {
			// A log this node had joined but not yet received anything
			// from: nothing to keep. If it takes the group over, it starts
			// the log afresh (RemoveNode makes it a bootstrap voter).
			bolt.Close()
			if err != nil {
				return err
			}
			continue
		}
		// RecoverCluster needs a transport only to encode addresses; an
		// in-memory one means recover doesn't need the cluster port.
		_, trans := raft.NewInmemTransport(raft.ServerAddress(o.Advertise))
		err = raft.RecoverCluster(c, &fsm{st: st, log: l}, bolt, bolt, snaps, trans, conf)
		bolt.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", l, err)
		}
	}
	return os.WriteFile(recoveredMark(st), []byte(o.ID+"\n"), 0o640)
}

// recoveredMark is the file Recover leaves for the next start's forget.
func recoveredMark(st *store.Store) string { return filepath.Join(st.Dir(), "raft", "recovered") }

func logf(format string, args ...any) { log.Printf("cluster: "+format, args...) }
