package cluster

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
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
	return Options{ID: id, Listen: addr, Advertise: addr, TLS: conf, LogOutput: io.Discard, tune: fast}
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

// TestReplicateAndRecover walks the M0 disaster drill end to end, over real
// mutual TLS on localhost:
//
//  1. n1 (the VPS) bootstraps as the only voter; studio joins as a non-voter.
//  2. Writes on n1 appear in studio's own SQLite files.
//  3. n1 "dies". studio's data directory is copied to a new node n1b, which
//     runs Recover and becomes the only voter, with every write intact.
//  4. n1b accepts new writes.
func TestReplicateAndRecover(t *testing.T) {
	caDir := testCA(t, "n1", "studio", "n1b")
	n1Addr, studioAddr := freeAddr(t), freeAddr(t)

	studioDir := t.TempDir()
	studioSt := openStore(t, studioDir)
	studio, err := Start(opts(t, caDir, "studio", "studio", studioAddr), studioSt)
	if err != nil {
		t.Fatal(err)
	}

	n1St := openStore(t, t.TempDir())
	o := opts(t, caDir, "n1", "n1", n1Addr)
	o.Bootstrap = true
	o.Peers = []config.Peer{{ID: "studio", Addr: studioAddr}}
	n1, err := Start(o, n1St)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "n1 to lead", n1.IsLeader)

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
	waitFor(t, "studio to have both groups", func() bool { return groupCount(t, studioSt) == 2 })

	// The group's own file arrived too, with its settings.
	gs, err := studioSt.GroupSettings(100)
	if err != nil || gs == nil || gs.Name != "travato" {
		t.Fatalf("studio's group file: %+v, %v", gs, err)
	}

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
	if err := Recover(opts(t, caDir, "n1b", "n1b", n1bAddr), n1bSt); err != nil {
		t.Fatal(err)
	}
	n1b, err := Start(opts(t, caDir, "n1b", "n1b", n1bAddr), n1bSt)
	if err != nil {
		t.Fatal(err)
	}
	defer n1b.Shutdown()
	waitFor(t, "n1b to lead", n1b.IsLeader)

	if n := groupCount(t, n1bSt); n != 2 {
		t.Fatalf("after recover: %d groups, want 2", n)
	}
	if _, err := n1b.Apply(&cmd.CreateGroup{GroupID: 102, Slug: "ekko", Name: "ekko", At: 2}); err != nil {
		t.Fatal(err)
	}
	if n := groupCount(t, n1bSt); n != 3 {
		t.Fatalf("after new write: %d groups, want 3", n)
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
	o.Bootstrap = true
	n1, err := Start(o, st)
	if err != nil {
		t.Fatal(err)
	}
	defer n1.Shutdown()

	intruder := opts(t, theirs, "intruder", "intruder", "")
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

// TestSnapshotRoundTrip checks that a snapshot restores into an identical
// set of databases.
func TestSnapshotRoundTrip(t *testing.T) {
	src := openStore(t, t.TempDir())
	log, err := NewLocal(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Apply(&cmd.CreateGroup{GroupID: 7, Slug: "travato", Name: "Travato", At: 1}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteSnapshot(src, &buf); err != nil {
		t.Fatal(err)
	}

	dst := openStore(t, t.TempDir())
	f := &fsm{st: dst}
	if err := f.Restore(io.NopCloser(&buf)); err != nil {
		t.Fatal(err)
	}
	if n := groupCount(t, dst); n != 1 {
		t.Fatalf("restored %d groups, want 1", n)
	}
	gs, err := dst.GroupSettings(7)
	if err != nil || gs == nil || gs.Name != "Travato" {
		t.Fatalf("restored group file: %+v, %v", gs, err)
	}
	// No temp directories left behind in the data directory.
	ents, _ := os.ReadDir(dst.Dir())
	for _, e := range ents {
		if e.Name() != "site.db" && e.Name() != "groups" && e.Name() != "site.db-wal" && e.Name() != "site.db-shm" {
			t.Errorf("leftover %s in data dir", e.Name())
		}
	}
}
