package cluster

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
)

// TestRPC runs the internal API on the same port as Raft (split by ALPN)
// and checks the three things it's for: moving photos between nodes, a
// follower forwarding a write to the leader, and tools looking up a group.
func TestRPC(t *testing.T) {
	caDir := testCA(t, "n1", "studio")
	n1Addr, studioAddr := freeAddr(t), freeAddr(t)

	studioSt := openStore(t, t.TempDir())
	studioOpts := opts(t, caDir, "studio", "studio", studioAddr)
	studio, err := Start(studioOpts, studioSt)
	if err != nil {
		t.Fatal(err)
	}
	defer studio.Shutdown()

	n1St := openStore(t, t.TempDir())
	o := opts(t, caDir, "n1", "n1", n1Addr)
	o.Bootstrap = true
	o.Peers = []config.Peer{{ID: "studio", Addr: studioAddr}}
	n1, err := Start(o, n1St)
	if err != nil {
		t.Fatal(err)
	}
	defer n1.Shutdown()
	waitFor(t, "n1 to lead", n1.IsLeader)

	n1Blobs, _ := blob.Open(t.TempDir())
	studioBlobs, _ := blob.Open(t.TempDir())
	go http.Serve(n1.RPCListener(), RPCHandler(n1, n1St, n1Blobs))
	go http.Serve(studio.RPCListener(), RPCHandler(studio, studioSt, studioBlobs))

	client := NewClient(studioOpts.TLS)

	// Push a photo to n1, then fetch it back.
	full, thumb := []byte("full-size bytes"), []byte("thumb")
	hash := blob.Hash(full)
	if err := client.PutBlob(n1Addr, hash, false, full); err != nil {
		t.Fatal(err)
	}
	if err := client.PutBlob(n1Addr, hash, true, thumb); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetBlob(n1Addr, hash, false)
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("GetBlob: %q, %v", got, err)
	}
	// A blob whose bytes don't match its name is refused.
	if err := client.PutBlob(n1Addr, blob.Hash([]byte("other")), false, full); err == nil {
		t.Fatal("mismatched blob accepted")
	}

	// Raft still works on the shared port: the studio joined as a
	// non-voter. Send a write to the studio; it redirects to n1.
	waitFor(t, "studio to follow", func() bool { return studio.LeaderAddr() == n1Addr })
	if _, err := client.Apply(studioAddr, &cmd.CreateGroup{GroupID: 7, Slug: "travato", Name: "Travato", At: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the group to reach the studio", func() bool { return groupCount(t, studioSt) == 1 })
	if id, err := client.GroupID(studioAddr, "travato"); err != nil || id != 7 {
		t.Fatalf("GroupID: %d, %v", id, err)
	}
	if _, err := client.GroupID(studioAddr, "nope"); err == nil {
		t.Fatal("unknown group found")
	}
	// A follower's own Apply forwards to the leader and returns only once
	// the follower has applied the write, so it can read it straight away.
	if _, err := studio.Apply(&cmd.CreateGroup{GroupID: 9, Slug: "ekko", Name: "Ekko", At: 1}); err != nil {
		t.Fatal(err)
	}
	if n := groupCount(t, studioSt); n != 2 {
		t.Fatalf("forwarded write not visible on the follower: %d groups", n)
	}
	// Errors keep their identity across the network.
	if _, err := studio.Apply(&cmd.CreateGroup{GroupID: 10, Slug: "ekko", Name: "dup", At: 1}); !errors.Is(err, cmd.ErrSlugTaken) || !cmd.IsInput(err) {
		t.Fatalf("forwarded error: %v", err)
	}
	if v := decodeValue([]byte("42")); v != int64(42) {
		t.Fatalf("decodeValue: %#v", v)
	}

	// Command errors come back as errors, not as a redirect loop.
	if _, err := client.Apply(n1Addr, &cmd.CreateGroup{GroupID: 8, Slug: "travato", Name: "dup", At: 1}); err == nil {
		t.Fatal("duplicate slug accepted over RPC")
	}
}
