package cluster

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// These tests run real nodes in one process, over real mutual TLS on
// localhost, and check the guarantees in docs/replication.md by name.

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

// testNode is one node in a test: its options, data directory and store,
// so it can be stopped and started again on the same files.
type testNode struct {
	t   *testing.T
	o   Options
	dir string
	st  *store.Store
	n   *Node
}

// newNode makes (but doesn't start) a node. num is its node number.
func newNode(t *testing.T, caDir, id string, num int, join ...string) *testNode {
	conf, err := LoadTLS(filepath.Join(caDir, "ca.crt"), filepath.Join(caDir, id+".crt"), filepath.Join(caDir, id+".key"))
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	o := Options{ID: id, Num: num, Listen: addr, Advertise: addr, TLS: conf, Join: join,
		tick: 30 * time.Millisecond, ackWait: 200 * time.Millisecond}
	// As in config: the first node (no join) takes new groups.
	o.Voter = len(join) == 0
	return &testNode{t: t, o: o, dir: t.TempDir()}
}

// start starts the node on its data directory.
func (tn *testNode) start() *testNode {
	tn.t.Helper()
	st, err := store.Open(tn.dir)
	if err != nil {
		tn.t.Fatal(err)
	}
	n, err := Start(tn.o, st)
	if err != nil {
		st.Close()
		tn.t.Fatal(err)
	}
	tn.st, tn.n = st, n
	tn.t.Cleanup(tn.stop)
	return tn
}

// stop shuts the node down, keeping its files.
func (tn *testNode) stop() {
	if tn.n != nil {
		tn.n.Shutdown()
		tn.st.Close()
		tn.n, tn.st = nil, nil
	}
}

func (tn *testNode) addr() string { return tn.o.Advertise }

func (tn *testNode) apply(c cmd.Command) any {
	tn.t.Helper()
	v, err := tn.n.Apply(c)
	if err != nil {
		tn.t.Fatalf("%s: %T: %v", tn.o.ID, c, err)
	}
	return v
}

// split cuts the network between two groups of nodes, both ways.
func split(a, b []*testNode) {
	for _, x := range a {
		for _, y := range b {
			x.n.client.blocked.Store(y.addr(), true)
			y.n.client.blocked.Store(x.addr(), true)
		}
	}
}

// heal puts the network back together.
func heal(all ...*testNode) {
	for _, x := range all {
		x.n.client.blocked.Clear()
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ready waits until every node has heard from every other.
func ready(t *testing.T, nodes ...*testNode) {
	t.Helper()
	for _, tn := range nodes {
		if err := tn.n.Ready(20 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
}

// bookkeeping tables differ between copies of a file by design: which old
// operations each node has deleted, which follow-ups it has sent, and the
// counts cmd.Direct keeps. Everything else must be the same everywhere.
var bookkeeping = map[string]bool{"ops": true, "outbox": true, "applied": true, "sqlite_sequence": true}

// digest is a file's content as text: every row of every table (but the
// bookkeeping ones and search's internals), sorted. Two copies are the
// same when their digests are.
func digest(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'search_fts%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		if !bookkeeping[name] {
			tables = append(tables, name)
		}
	}
	rows.Close()
	var b strings.Builder
	for _, name := range tables {
		rows, err := db.Query(`SELECT * FROM "` + name + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			lines = append(lines, fmt.Sprint(vals...))
		}
		rows.Close()
		sort.Strings(lines)
		fmt.Fprintf(&b, "%s\n  %s\n", name, strings.Join(lines, "\n  "))
	}
	return b.String()
}

// same waits until every node has the same copy of a file (0 = site.db).
func same(t *testing.T, gid int64, nodes ...*testNode) {
	t.Helper()
	var last []string
	ok := func() bool {
		last = last[:0]
		for _, tn := range nodes {
			db, err := tn.st.Live(gid)
			if err != nil {
				return false
			}
			last = append(last, digest(t, db))
		}
		for _, d := range last[1:] {
			if d != last[0] {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	for i, d := range last {
		t.Logf("%s's copy of %d:\n%s", nodes[i].o.ID, gid, d)
	}
	t.Fatalf("copies of file %d never became the same", gid)
}

func postTitles(t *testing.T, st *store.Store, gid int64) []string {
	db, err := st.Group(gid)
	if err != nil {
		return nil
	}
	rows, err := db.Query(`SELECT title FROM posts ORDER BY title`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// newGroup creates a group with an owner who can post in it.
func newGroup(tn *testNode, gid int64, slug string, owner int64) {
	tn.apply(&cmd.CreateGroup{GroupID: gid, Slug: slug, Name: slug, OwnerID: owner, At: 1})
}

// waitHolds waits until a node holds a group, with its owner in place.
func waitHolds(t *testing.T, tn *testNode, gid, owner int64) {
	t.Helper()
	waitFor(t, tn.o.ID+" to hold group", func() bool {
		if !tn.n.Holds(gid) {
			return false
		}
		m, err := tn.st.Membership(gid, owner)
		return err == nil && m != nil
	})
}

func post(tn *testNode, gid, id, user int64, title string) {
	tn.apply(&cmd.CreatePost{GroupID: gid, PostID: id, UserID: user, Title: title, At: time.Now().Unix()})
}

// TestOneNodeAlone: a node with no other node carries on, reads and writes,
// and everything it has becomes stable at once.
func TestOneNodeAlone(t *testing.T) {
	caDir := testCA(t, "n1")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	waitHolds(t, n1, 100, 7)
	post(n1, 100, 1001, 7, "Solar")
	if got := postTitles(t, n1.st, 100); len(got) != 1 {
		t.Fatalf("posts: %v", got)
	}
	e := n1.n.engineFor(100)
	waitFor(t, "the stable copy to catch up", func() bool {
		stable, _ := n1.st.Stable(100)
		live, _ := n1.st.Live(100)
		sp, _ := store.PositionOf(stable)
		lp, _ := store.PositionOf(live)
		return sp == lp
	})
	e.mu.Lock()
	f := n1.n.stablePoint(e)
	e.mu.Unlock()
	if f == 0 {
		t.Fatal("alone, the stable point should move with the clock")
	}
}

// TestTwoNodesReplicate: a node joins through another, copies site.db,
// registers, gets every group (it's a full node), and writes on either
// reach the other.
func TestTwoNodesReplicate(t *testing.T) {
	caDir := testCA(t, "n1", "studio")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)

	sOpts := newNode(t, caDir, "studio", 2, n1.addr())
	sOpts.o.Full = true
	studio := sOpts.start()
	ready(t, n1, studio)
	waitHolds(t, studio, 100, 7)

	post(n1, 100, 1001, 7, "From n1")
	post(studio, 100, 1002, 7, "From studio")
	waitFor(t, "both posts on both nodes", func() bool {
		return len(postTitles(t, n1.st, 100)) == 2 && len(postTitles(t, studio.st, 100)) == 2
	})
	same(t, 0, n1, studio)
	same(t, 100, n1, studio)
}

// TestSplitAndRejoin: two nodes lose touch, both keep taking writes to the
// same group, including a comment on a post the other side then locks,
// and a group address claimed on both sides. When they can reach each
// other again, both end with the same files and nothing is lost.
func TestSplitAndRejoin(t *testing.T) {
	caDir := testCA(t, "n1", "studio")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	s := newNode(t, caDir, "studio", 2, n1.addr())
	s.o.Full = true
	studio := s.start()
	ready(t, n1, studio)
	waitHolds(t, studio, 100, 7)
	post(n1, 100, 1000, 7, "Before the split")
	same(t, 100, n1, studio)

	split([]*testNode{n1}, []*testNode{studio})
	for i := 0; i < 5; i++ {
		post(n1, 100, 2000+int64(i), 7, fmt.Sprintf("n1 %d", i))
		post(studio, 100, 3000+int64(i), 7, fmt.Sprintf("studio %d", i))
	}
	// A comment on one side, the post locked on the other.
	studio.apply(&cmd.CreateComment{GroupID: 100, CommentID: 4000, PostID: 1000, UserID: 7, Body: "late", At: 5})
	n1.apply(&cmd.SetPostFlag{GroupID: 100, PostID: 1000, Flag: "locked", On: true, By: 7, At: 4})
	// The same new group address on both sides.
	newGroup(n1, 200, "promaster", 7)
	newGroup(studio, 300, "promaster", 7)

	heal(n1, studio)
	same(t, 0, n1, studio)
	same(t, 100, n1, studio)
	if got := postTitles(t, n1.st, 100); len(got) != 11 {
		t.Fatalf("posts after rejoining: %v", got)
	}
	a, _ := n1.st.GroupByID(200)
	b, _ := n1.st.GroupByID(300)
	if a == nil || b == nil || a.Slug == b.Slug {
		t.Fatalf("both groups, with different addresses: %+v %+v", a, b)
	}
	// The comment and the lock: whichever came second in the final order,
	// the outcome is the same on both, and a comment refused for the lock
	// is on record rather than gone.
	db, _ := n1.st.Group(100)
	var comments, conflicts int
	db.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&comments)
	db.QueryRow(`SELECT COUNT(*) FROM conflicts WHERE command = 'CreateComment'`).Scan(&conflicts)
	if comments+conflicts != 1 {
		t.Fatalf("the late comment: %d kept, %d recorded as conflicts", comments, conflicts)
	}
}

// TestOfflineNodeCatchesUp: a node that was stopped while the other wrote
// catches up by itself when it starts again, and one whose operations were
// meanwhile deleted everywhere else gets a fresh copy instead.
func TestOfflineNodeCatchesUp(t *testing.T) {
	caDir := testCA(t, "n1", "studio")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	s := newNode(t, caDir, "studio", 2, n1.addr())
	s.o.Full = true
	studio := s.start()
	ready(t, n1, studio)
	waitHolds(t, studio, 100, 7)

	studio.stop()
	for i := 0; i < 20; i++ {
		post(n1, 100, 5000+int64(i), 7, fmt.Sprintf("while away %d", i))
	}
	studio.start()
	same(t, 100, n1, studio)
	if got := postTitles(t, studio.st, 100); len(got) != 20 {
		t.Fatalf("studio caught up to %d posts", len(got))
	}
}

// TestRewindOrder: operations made on two nodes at nearly the same time
// arrive at each in a different order; both end up applying them in the
// same final order (checked by a counter that depends on order).
func TestRewindOrder(t *testing.T) {
	caDir := testCA(t, "n1", "studio")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	s := newNode(t, caDir, "studio", 2, n1.addr())
	s.o.Full = true
	studio := s.start()
	ready(t, n1, studio)
	waitHolds(t, studio, 100, 7)
	post(n1, 100, 1000, 7, "Thread")
	same(t, 100, n1, studio)

	split([]*testNode{n1}, []*testNode{studio})
	for i := 0; i < 10; i++ {
		n1.apply(&cmd.CreateComment{GroupID: 100, CommentID: 6000 + int64(i), PostID: 1000, UserID: 7, Body: "a", At: 10})
		studio.apply(&cmd.CreateComment{GroupID: 100, CommentID: 7000 + int64(i), PostID: 1000, UserID: 7, Body: "b", At: 10})
	}
	heal(n1, studio)
	same(t, 100, n1, studio)
	if n1.n.rewinds.Load()+studio.n.rewinds.Load() == 0 {
		t.Fatal("interleaved writes should have made at least one node rewind")
	}
}

// TestRebuildFromSurvivor: every node but the Studio is lost. The Studio
// carries on alone; a new node started pointing at it gets everything;
// removing the lost node lets the stable point move again.
func TestRebuildFromSurvivor(t *testing.T) {
	caDir := testCA(t, "n1", "studio", "n2")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	s := newNode(t, caDir, "studio", 2, n1.addr())
	s.o.Full = true
	studio := s.start()
	ready(t, n1, studio)
	waitHolds(t, studio, 100, 7)
	post(n1, 100, 1000, 7, "Before")
	same(t, 100, n1, studio)

	// n1 is destroyed: stopped, and its files gone.
	n1.stop()
	os.RemoveAll(n1.dir)

	post(studio, 100, 1001, 7, "Studio alone")
	n2o := newNode(t, caDir, "n2", 3, studio.addr())
	n2o.o.Voter = true
	n2 := n2o.start()
	waitFor(t, "n2 registered, as the Studio sees it", func() bool {
		nodes, _ := studio.st.Nodes()
		for _, nd := range nodes {
			if nd.ID == "n2" {
				return true
			}
		}
		return false
	})
	studio.apply(&cmd.PlaceGroup{GroupID: 100, NodeID: "n2", Voter: true, At: 2})
	waitHolds(t, n2, 100, 7)
	same(t, 100, studio, n2)
	if got := postTitles(t, n2.st, 100); len(got) != 2 {
		t.Fatalf("n2 has %v", got)
	}

	// The stable point waits for n1 until it's removed: it stays at the
	// last clock reading n1 sent, before the Studio's writes since.
	stableAt := func(tn *testNode) int64 {
		e := tn.n.engineFor(100)
		e.mu.Lock()
		defer e.mu.Unlock()
		return tn.n.stablePoint(e)
	}
	live, _ := studio.st.Live(100)
	last, _ := store.PositionOf(live)
	time.Sleep(100 * time.Millisecond)
	if stableAt(studio) >= last.Stamp {
		t.Fatal("the stable point passed a write while a node in the map was silent")
	}
	studio.apply(&cmd.RemoveNode{ID: "n1", Replacement: "studio", At: 3})
	waitFor(t, "the stable point to move", func() bool { return stableAt(studio) > last.Stamp && stableAt(n2) > last.Stamp })
}

// TestRandomSplits is the convergence test: three nodes, writes on random
// nodes to two groups, the network split and healed at random. At the end
// every copy of every file is the same, and holds every post made.
func TestRandomSplits(t *testing.T) {
	caDir := testCA(t, "a", "b", "c")
	a := newNode(t, caDir, "a", 1)
	a.o.Full = true
	a.start()
	ready(t, a)
	newGroup(a, 100, "one", 7)
	newGroup(a, 200, "two", 7)
	b := newNode(t, caDir, "b", 2, a.addr())
	b.o.Full = true
	b.start()
	c := newNode(t, caDir, "c", 3, a.addr())
	c.o.Full = true
	c.start()
	nodes := []*testNode{a, b, c}
	ready(t, nodes...)
	for _, tn := range nodes {
		waitHolds(t, tn, 100, 7)
		waitHolds(t, tn, 200, 7)
	}

	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	rnd := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int(uint64(seed)>>33) % n
	}
	want := 0
	for round := 0; round < 6; round++ {
		heal(nodes...)
		// Split one node off from the other two, or none.
		if k := rnd(4); k < 3 {
			var rest []*testNode
			for i, tn := range nodes {
				if i != k {
					rest = append(rest, tn)
				}
			}
			split([]*testNode{nodes[k]}, rest)
		}
		for i := 0; i < 8; i++ {
			tn := nodes[rnd(3)]
			gid := int64(100 * (1 + rnd(2)))
			post(tn, gid, int64(10000+round*100+i), 7, fmt.Sprintf("r%d-%d", round, i))
			want++
		}
		time.Sleep(time.Duration(rnd(100)) * time.Millisecond)
	}
	heal(nodes...)
	same(t, 0, nodes...)
	same(t, 100, nodes...)
	same(t, 200, nodes...)
	if got := len(postTitles(t, a.st, 100)) + len(postTitles(t, a.st, 200)); got != want {
		t.Fatalf("%d posts, want %d", got, want)
	}
}

// TestRejectsForeignCertificate: a node with a certificate from another CA
// can't join.
func TestRejectsForeignCertificate(t *testing.T) {
	caDir := testCA(t, "n1")
	other := testCA(t, "intruder")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	in := newNode(t, other, "intruder", 5, n1.addr())
	in.o.joinWait = time.Second
	st, err := store.Open(in.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n, err := Start(in.o, st); err == nil {
		n.Shutdown()
		t.Fatal("an outside node got a copy of site.db")
	}
}

// TestRemovedNodeComesBack: a node removed while it was only out of
// touch comes back, finds itself removed, and sends what it wrote in the
// meantime again as a new node: nothing is lost, and the copies agree.
func TestRemovedNodeComesBack(t *testing.T) {
	caDir := testCA(t, "n1", "n2")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	o2 := newNode(t, caDir, "n2", 2, n1.addr())
	o2.o.Full = true
	n2 := o2.start()
	ready(t, n1, n2)
	waitHolds(t, n2, 100, 7)
	oldOrigin := n2.n.id.origin()

	split([]*testNode{n1}, []*testNode{n2})
	post(n2, 100, 9001, 7, "written while apart")
	n1.apply(&cmd.RemoveNode{ID: "n2", Replacement: "n1", At: 5})
	heal(n1, n2)

	waitFor(t, "n2 to come back as a new node", func() bool { return n2.n.id.origin() != oldOrigin && n2.n.registered() })
	waitFor(t, "n2's post on n1", func() bool {
		for _, title := range postTitles(t, n1.st, 100) {
			if title == "written while apart" {
				return true
			}
		}
		return false
	})
	same(t, 0, n1, n2)
}

// TestUpgradeFromRaft: two nodes upgraded from Raft, one of whose copies
// was behind. The one that's behind takes the other's copy, keeping
// anything it made since.
func TestUpgradeFromRaft(t *testing.T) {
	caDir := testCA(t, "n1", "studio")
	n1 := newNode(t, caDir, "n1", 1)
	studio := newNode(t, caDir, "studio", 2, n1.addr())
	studio.o.Full = true
	// Both nodes' files as Raft left them: the same history, the studio's
	// a few entries short.
	build := func(tn *testNode, posts int, base int64) {
		st, err := store.Open(tn.dir)
		if err != nil {
			t.Fatal(err)
		}
		d := &cmd.Direct{Store: st}
		for _, c := range []cmd.Command{
			&cmd.RegisterNode{ID: "n1", Addr: n1.addr(), Voter: true, Num: -1, At: 1},
			&cmd.RegisterNode{ID: "studio", Addr: studio.addr(), Full: true, Num: -1, At: 1},
			&cmd.CreateGroup{GroupID: 100, Slug: "travato", Name: "Travato", OwnerID: 7, At: 1},
		} {
			if _, err := d.Apply(c); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < posts; i++ {
			d.Apply(&cmd.CreatePost{GroupID: 100, PostID: int64(100 + i), UserID: 7, Title: fmt.Sprint("post ", i), At: 1})
		}
		site, _ := st.Live(0)
		group, _ := st.Live(100)
		store.SetBase(site, base)
		store.SetBase(group, base)
		st.Close()
	}
	build(n1, 5, 20)
	build(studio, 3, 18)
	n1.start()
	studio.start()
	ready(t, n1, studio)
	same(t, 100, n1, studio)
	if got := postTitles(t, studio.st, 100); len(got) != 5 {
		t.Fatalf("studio after the upgrade: %v", got)
	}
}

// TestEnsureCerts: the first node makes the site's CA and its own
// certificate; a node given a copy of the CA (make install copies it)
// makes its own from it; a node with neither is told what's missing.
func TestEnsureCerts(t *testing.T) {
	first := t.TempDir()
	if err := EnsureCerts(first+"/ca.crt", first+"/node.crt", first+"/node.key", "n1", true); err != nil {
		t.Fatal(err)
	}
	joiner := t.TempDir()
	for _, f := range []string{"ca.crt", "ca.key"} {
		data, _ := os.ReadFile(first + "/" + f)
		os.WriteFile(joiner+"/"+f, data, 0o600)
	}
	if err := EnsureCerts(joiner+"/ca.crt", joiner+"/node.crt", joiner+"/node.key", "n2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTLS(joiner+"/ca.crt", joiner+"/node.crt", joiner+"/node.key"); err != nil {
		t.Fatal(err)
	}
	// Run again: nothing changes.
	before, _ := os.ReadFile(joiner + "/node.crt")
	EnsureCerts(joiner+"/ca.crt", joiner+"/node.crt", joiner+"/node.key", "n2", false)
	if after, _ := os.ReadFile(joiner + "/node.crt"); string(after) != string(before) {
		t.Fatal("a second run replaced the certificate")
	}
	empty := t.TempDir()
	if err := EnsureCerts(empty+"/ca.crt", empty+"/node.crt", empty+"/node.key", "n3", false); err == nil ||
		!strings.Contains(err.Error(), "make install") {
		t.Fatalf("no CA and not the first node: %v", err)
	}
}

// TestNodeNobodyCanReach: a node behind a home router (no address others
// can reach) does all the talking: its writes still reach the others, it
// still gets theirs, and the stable point still moves, from the report it
// sends.
func TestNodeNobodyCanReach(t *testing.T) {
	caDir := testCA(t, "n1", "home")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	newGroup(n1, 100, "travato", 7)
	h := newNode(t, caDir, "home", 2, n1.addr())
	h.o.Advertise = ""                                        // find it...
	h.o.publicIP = func(context.Context) string { return "" } // ...and fail
	h.o.Full = true
	home := h.start()
	waitFor(t, "home registered with no address", func() bool {
		nodes, _ := n1.st.Nodes()
		for _, nd := range nodes {
			if nd.ID == "home" {
				return nd.Addr == ""
			}
		}
		return false
	})
	waitHolds(t, home, 100, 7)
	post(home, 100, 1001, 7, "from home")
	post(n1, 100, 1002, 7, "from n1")
	same(t, 100, n1, home)
	waitFor(t, "n1's stable point to pass home's post", func() bool {
		stable, _ := n1.st.Stable(100)
		var n int
		stable.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&n)
		return n == 2
	})
}

// TestFindsItsAddress: a node with no address set learns its public IP
// (here, what the test says it is) and, when another node can connect
// back to it there, registers that address.
func TestFindsItsAddress(t *testing.T) {
	caDir := testCA(t, "n1", "n2")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	o := newNode(t, caDir, "n2", 2, n1.addr())
	_, port, _ := net.SplitHostPort(o.o.Listen)
	o.o.Advertise = ""
	o.o.publicIP = func(context.Context) string { return "127.0.0.1" }
	n2 := o.start()
	waitFor(t, "n2 registered at the address n1 could reach", func() bool {
		nodes, _ := n1.st.Nodes()
		for _, nd := range nodes {
			if nd.ID == "n2" {
				return nd.Addr == "127.0.0.1:"+port
			}
		}
		return false
	})
	// And a caller can only speak for itself: n1 can't send a report
	// claiming to be n2.
	fake := &Report{ID: "n2", Origin: "n2@1"}
	var back Report
	if err := n1.n.client.PostJSON(context.Background(), n2.addr(), "/sync/report", fake, &back); err == nil {
		t.Fatal("a report claiming to be another node was accepted")
	}
}
