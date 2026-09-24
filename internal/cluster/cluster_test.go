package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// fast shortens Raft's timeouts so a single-node test cluster elects itself
// in milliseconds instead of a second or two.
func fast(c *raft.Config) {
	c.HeartbeatTimeout = 100 * time.Millisecond
	c.ElectionTimeout = 100 * time.Millisecond
	c.LeaderLeaseTimeout = 50 * time.Millisecond
	c.CommitTimeout = 5 * time.Millisecond
}

// freeAddr finds a free localhost port.
func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// testCA makes a cluster CA and certificates for the given node ids.
func testCA(t *testing.T, ids ...string) string {
	dir := t.TempDir()
	if err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := IssueNodeCert(dir, id); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func opts(t *testing.T, caDir, certID, id, addr string) Options {
	conf, err := LoadTLS(filepath.Join(caDir, "ca.crt"), filepath.Join(caDir, certID+".crt"), filepath.Join(caDir, certID+".key"))
	if err != nil {
		t.Fatal(err)
	}
	return Options{ID: id, Listen: addr, Advertise: addr, TLS: conf, LogOutput: io.Discard, tune: fast, tick: 50 * time.Millisecond}
}

// startNode starts a node and waits until it's registered and holds what
// the map places on it.
func startNode(t *testing.T, o Options, st *store.Store) *Node {
	n, err := Start(o, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Ready(20 * time.Second); err != nil {
		n.Shutdown()
		t.Fatal(err)
	}
	return n
}

func openStore(t *testing.T, dir string) *store.Store {
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func groupCount(t *testing.T, st *store.Store) int {
	gs, err := st.Groups()
	if err != nil {
		t.Fatal(err)
	}
	return len(gs)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestReplicateAndRecover walks the disaster drill end to end, over real
// mutual TLS on localhost:
//
//  1. n1 (the VPS) bootstraps as the only voter; studio joins as a
//     full-copy non-voter.
//  2. Writes on n1 appear in studio's own SQLite files: site.db, and each
//     group's file, through that group's own log.
//  3. n1 "dies". studio's data directory is copied to a new node n1b, which
//     runs Recover and becomes the only voter of every log, with every
//     write intact.
//  4. n1b accepts new writes, including a new group.
func TestReplicateAndRecover(t *testing.T) {
	caDir := testCA(t, "n1", "studio", "n1b")
	n1Addr, studioAddr := freeAddr(t), freeAddr(t)

	n1St := openStore(t, t.TempDir())
	o := opts(t, caDir, "n1", "n1", n1Addr)
	o.Bootstrap, o.Voter = true, true
	n1 := startNode(t, o, n1St)

	studioDir := t.TempDir()
	studioSt := openStore(t, studioDir)
	so := opts(t, caDir, "studio", "studio", studioAddr)
	so.Full, so.Join = true, []string{n1Addr}
	studio := startNode(t, so, studioSt)

	for i, slug := range []string{"travato", "promaster"} {
		c := &cmd.CreateGroup{GroupID: int64(100 + i), Slug: slug, Name: slug, At: 1}
		if _, err := n1.Apply(c); err != nil {
			t.Fatal(err)
		}
	}
	// A failing command fails the same way everywhere and changes nothing.
	if _, err := n1.Apply(&cmd.CreateGroup{GroupID: 999, Slug: "travato", Name: "dup", At: 1}); err == nil {
		t.Fatal("duplicate slug was accepted")
	}
	// The group's own file is written on n1 by the time Apply returns (n1
	// leads every log here, and relays the group's first settings itself).
	if gs, err := n1St.GroupSettings(100); err != nil || gs == nil || gs.Name != "travato" {
		t.Fatalf("n1's group file: %+v, %v", gs, err)
	}
	waitFor(t, "studio to have both groups", func() bool { return groupCount(t, studioSt) == 2 })
	// A group write goes through the group's log, which studio follows.
	if _, err := studio.Apply(&cmd.UpdateSettings{GroupID: 100, Set: map[string]any{"name": "Travato"}, At: 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "studio's group file", func() bool {
		gs, err := studioSt.GroupSettings(100)
		return err == nil && gs != nil && gs.Name == "Travato"
	})
	// ...and the new name came back to site.db through the outbox.
	waitFor(t, "the name in studio's site.db", func() bool {
		g, err := studioSt.GroupByID(100)
		return err == nil && g != nil && g.Name == "Travato"
	})

	// n1 is gone. Stop studio too (as `grus recover` requires), and copy its
	// data directory to the replacement node.
	n1.Shutdown()
	n1St.Close()
	studio.Shutdown()
	studioSt.Close()

	n1bDir := t.TempDir()
	if out, err := exec.Command("cp", "-a", studioDir+"/.", n1bDir).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}
	n1bAddr := freeAddr(t)
	n1bSt := openStore(t, n1bDir)
	bo := opts(t, caDir, "n1b", "n1b", n1bAddr)
	bo.Voter = true
	if err := Recover(bo, n1bSt); err != nil {
		t.Fatal(err)
	}
	n1b := startNode(t, bo, n1bSt)
	defer n1b.Shutdown()
	waitFor(t, "n1b to lead", n1b.IsLeader)

	if n := groupCount(t, n1bSt); n != 2 {
		t.Fatalf("after recover: %d groups, want 2", n)
	}
	// n1 is out of the map, and n1b has taken its groups.
	nodes, _ := n1bSt.Nodes()
	for _, nd := range nodes {
		if nd.ID == "n1" {
			t.Fatal("n1 still in the node map")
		}
	}
	waitFor(t, "n1b to lead group 100", func() bool { return n1b.Leads(100) })
	if _, err := n1b.Apply(&cmd.UpdateSettings{GroupID: 100, Set: map[string]any{"name": "Travato owners"}, At: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := n1b.Apply(&cmd.CreateGroup{GroupID: 102, Slug: "ekko", Name: "ekko", At: 2}); err != nil {
		t.Fatal(err)
	}
	if n := groupCount(t, n1bSt); n != 3 {
		t.Fatalf("after new write: %d groups, want 3", n)
	}
	if gs, err := n1bSt.GroupSettings(102); err != nil || gs == nil || gs.Name != "ekko" {
		t.Fatalf("new group's file after recover: %+v, %v", gs, err)
	}
}

// TestRejectsForeignCertificate checks that a node with a certificate from a
// different CA can't talk to the cluster.
func TestRejectsForeignCertificate(t *testing.T) {
	ours := testCA(t, "n1")
	theirs := testCA(t, "intruder")
	addr := freeAddr(t)

	st := openStore(t, t.TempDir())
	o := opts(t, ours, "n1", "n1", addr)
	o.Bootstrap, o.Voter = true, true
	n1 := startNode(t, o, st)
	defer n1.Shutdown()

	intruder := opts(t, theirs, "intruder", "intruder", "")
	intruder.TLS.NextProtos = []string{raftProto(cmd.SiteLog)}
	s := &tlsStream{conf: intruder.TLS}
	conn, err := s.Dial(raft.ServerAddress(addr), time.Second)
	if err == nil {
		// TLS 1.3 may report the server's rejection on the first read.
		conn.SetDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(make([]byte, 1))
		conn.Close()
	}
	if err == nil {
		t.Fatal("connection with a foreign certificate was accepted")
	}
}

// TestSnapshotRoundTrip checks that a log's snapshot restores into an
// identical file: site.db for the site log, a group's file for its log.
func TestSnapshotRoundTrip(t *testing.T) {
	src := openStore(t, t.TempDir())
	log, err := NewLocal(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Apply(&cmd.CreateGroup{GroupID: 7, Slug: "travato", Name: "Travato", At: 1}); err != nil {
		t.Fatal(err)
	}
	dst := openStore(t, t.TempDir())
	for _, l := range []cmd.LogID{cmd.SiteLog, 7} {
		var buf bytes.Buffer
		if err := WriteSnapshot(src, l, &buf); err != nil {
			t.Fatal(err)
		}
		f := &fsm{st: dst, log: l}
		if err := f.Restore(io.NopCloser(&buf)); err != nil {
			t.Fatal(err)
		}
	}
	if n := groupCount(t, dst); n != 1 {
		t.Fatalf("restored %d groups, want 1", n)
	}
	gs, err := dst.GroupSettings(7)
	if err != nil || gs == nil || gs.Name != "Travato" {
		t.Fatalf("restored group file: %+v, %v", gs, err)
	}
	// No temp files left behind in the data directory.
	ents, _ := os.ReadDir(dst.Dir())
	for _, e := range ents {
		if e.Name() != "site.db" && e.Name() != "groups" && e.Name() != "site.db-wal" && e.Name() != "site.db-shm" {
			t.Errorf("leftover %s in data dir", e.Name())
		}
	}
}

// TestPlacementAndFailover runs four nodes the way a grown site would: three
// voters (a, b, c) and a small node d that holds only what it's given. It
// checks that a new group lands on the three voters and not on d, that d
// can still write to it (forwarded to the group's leader), that placing
// the group on d copies it there and taking it off deletes d's copy, and
// that killing the group's leader leaves the other two carrying on.
func TestPlacementAndFailover(t *testing.T) {
	caDir := testCA(t, "a", "b", "c", "d")
	addrs := map[string]string{}
	for _, id := range []string{"a", "b", "c", "d"} {
		addrs[id] = freeAddr(t)
	}
	nodes := map[string]*Node{}
	stores := map[string]*store.Store{}
	for _, id := range []string{"a", "b", "c", "d"} {
		o := opts(t, caDir, id, id, addrs[id])
		o.Voter = id != "d"
		o.Bootstrap = id == "a"
		if id != "a" {
			o.Join = []string{addrs["a"]}
		}
		stores[id] = openStore(t, t.TempDir())
		nodes[id] = startNode(t, o, stores[id])
	}
	defer func() {
		for _, n := range nodes {
			n.Shutdown()
		}
	}()
	// b and c were added to the site log as non-voters, then promoted.
	waitFor(t, "three site voters", func() bool {
		f := nodes["a"].site().raft.GetConfiguration()
		v := 0
		for _, s := range f.Configuration().Servers {
			if s.Suffrage == raft.Voter {
				v++
			}
		}
		return f.Error() == nil && v == 3
	})

	if _, err := nodes["a"].Apply(&cmd.CreateGroup{GroupID: 11, Slug: "travato", Name: "Travato", At: 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		n := nodes[id]
		waitFor(t, id+" to hold the group", func() bool { return n.Holds(11) })
	}
	if nodes["d"].Holds(11) || stores["d"].HasGroup(11) {
		t.Fatal("d holds a group nobody placed on it")
	}
	// d writes to a group it doesn't hold: forwarded to the group's leader.
	rename := func(from, name string) error {
		_, err := nodes[from].Apply(&cmd.UpdateSettings{GroupID: 11, Set: map[string]any{"name": name}, At: 2})
		return err
	}
	if err := rename("d", "Travato owners"); err != nil {
		t.Fatal(err)
	}
	if stores["d"].HasGroup(11) {
		t.Fatal("forwarding a write left a group file on d")
	}
	// The site's copy of the name reaches d through the site log.
	waitFor(t, "the new name in d's site.db", func() bool {
		g, _ := stores["d"].GroupByID(11)
		return g != nil && g.Name == "Travato owners"
	})

	// A web request to d for the group is passed on to a node that holds
	// it, with the site's host name and the visitor's address kept.
	for _, id := range []string{"a", "b", "c"} {
		id := id
		nodes[id].ServeWeb(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s|%s|%s|%s", id, r.Host, r.URL.RequestURI(), r.RemoteAddr)
		}))
	}
	req := httptest.NewRequest("GET", "https://travato.nfb.group/p/5?sort=top", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	if !nodes["d"].PassOn(rec, req, 11) {
		t.Fatal("d didn't pass the request on")
	}
	if got := rec.Body.String(); !strings.Contains(got, "|travato.nfb.group|/p/5?sort=top|203.0.113.9:") {
		t.Fatalf("passed-on request arrived as %q", got)
	}
	// No full-copy node here, so the pages that gather from every group
	// have nowhere to go.
	if nodes["d"].PassOn(httptest.NewRecorder(), req, 0) {
		t.Fatal("passed on to a full node that doesn't exist")
	}

	// Place the group on d: d copies it from the group's leader.
	if _, err := nodes["d"].Apply(&cmd.PlaceGroup{GroupID: 11, NodeID: "d", At: 3}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "d's copy of the group", func() bool {
		gs, err := stores["d"].GroupSettings(11)
		return err == nil && gs != nil && gs.Name == "Travato owners"
	})
	// ...and take it off again: d's copy goes.
	if _, err := nodes["a"].Apply(&cmd.UnplaceGroup{GroupID: 11, NodeID: "d", At: 4}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "d to drop the group", func() bool { return !nodes["d"].Holds(11) && !stores["d"].HasGroup(11) })

	// Kill the group's leader. The other two elect a new one, and writes
	// (from d, which knows only the map) carry on.
	var dead string
	waitFor(t, "a group leader", func() bool {
		for _, id := range []string{"a", "b", "c"} {
			if nodes[id].Leads(11) {
				dead = id
				return true
			}
		}
		return false
	})
	nodes[dead].Shutdown()
	delete(nodes, dead)
	waitFor(t, "a write after the leader died", func() bool { return rename("d", "Travato") == nil })
	for id, st := range stores {
		if id == dead || id == "d" {
			continue
		}
		waitFor(t, id+" to have the write", func() bool {
			gs, _ := st.GroupSettings(11)
			return gs != nil && gs.Name == "Travato"
		})
	}
	// The site log survives too (it may have lost its leader as well).
	waitFor(t, "a site write after the leader died", func() bool {
		_, err := nodes["d"].Apply(&cmd.CreateGroup{GroupID: 12, Slug: "ekko", Name: "Ekko", At: 5})
		return err == nil || errors.Is(err, cmd.ErrSlugTaken)
	})
}

// TestWritesSurviveLeaderKill is the "kill nodes during a load test" drill
// (plan, M7): three voters take a steady stream of posts to one group,
// through all three nodes at once, and the group's leader is killed partway
// through. Writes must carry on through the other two, and every write
// that was acknowledged, before or after the kill, must be on both
// survivors: an acknowledged write is never lost.
func TestWritesSurviveLeaderKill(t *testing.T) {
	caDir := testCA(t, "a", "b", "c")
	ids := []string{"a", "b", "c"}
	nodes := map[string]*Node{}
	stores := map[string]*store.Store{}
	var addrA string
	for _, id := range ids {
		addr := freeAddr(t)
		o := opts(t, caDir, id, id, addr)
		o.Voter = true
		if id == "a" {
			o.Bootstrap, addrA = true, addr
		} else {
			o.Join = []string{addrA}
		}
		stores[id] = openStore(t, t.TempDir())
		nodes[id] = startNode(t, o, stores[id])
	}
	defer func() {
		for _, n := range nodes {
			n.Shutdown()
		}
	}()
	const gid, owner = 21, 7
	if _, err := nodes["a"].Apply(&cmd.CreateGroup{GroupID: gid, Slug: "travato", Name: "Travato", OwnerID: owner, At: 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		st := stores[id]
		waitFor(t, id+" to have the group's owner", func() bool {
			m, _ := st.Membership(gid, owner)
			return m != nil
		})
	}

	var (
		mu     sync.Mutex
		acked  []int64
		dead   = map[string]bool{}
		nextID atomic.Int64
		after  atomic.Int64 // writes acknowledged after the kill
		killed atomic.Bool
		stop   = make(chan struct{})
		wg     sync.WaitGroup
	)
	nextID.Store(1000)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				mu.Lock()
				gone := dead[id]
				mu.Unlock()
				if gone {
					return
				}
				pid := nextID.Add(1)
				_, err := nodes[id].Apply(&cmd.CreatePost{GroupID: gid, PostID: pid, UserID: owner, Title: fmt.Sprint("post ", pid), At: 2})
				if err != nil {
					continue // not acknowledged: it may or may not have landed
				}
				mu.Lock()
				acked = append(acked, pid)
				mu.Unlock()
				if killed.Load() {
					after.Add(1)
				}
			}
		}(id)
	}

	time.Sleep(500 * time.Millisecond)
	var victim string
	waitFor(t, "a group leader", func() bool {
		for _, id := range ids {
			if nodes[id].Leads(gid) {
				victim = id
				return true
			}
		}
		return false
	})
	mu.Lock()
	dead[victim] = true
	mu.Unlock()
	nodes[victim].Shutdown()
	killed.Store(true)
	waitFor(t, "writes to resume after the kill", func() bool { return after.Load() >= 20 })
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(acked) < 40 {
		t.Fatalf("only %d writes acknowledged", len(acked))
	}
	for _, id := range ids {
		if id == victim {
			continue
		}
		st := stores[id]
		waitFor(t, id+" to have every acknowledged write", func() bool {
			db, err := st.Group(gid)
			if err != nil {
				return false
			}
			for _, pid := range acked {
				var n int
				if db.QueryRow(`SELECT COUNT(*) FROM posts WHERE id = ?`, pid).Scan(&n); n != 1 {
					return false
				}
			}
			return true
		})
	}
	t.Logf("%d writes acknowledged, %d after killing %s", len(acked), after.Load(), victim)
}
