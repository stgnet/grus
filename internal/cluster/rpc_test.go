package cluster

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cmd"
)

// TestRPC checks the internal API's other jobs: moving photos between
// nodes, a node passing a write for a group it doesn't hold to one that
// does, and tools looking up a group.
func TestRPC(t *testing.T) {
	caDir := testCA(t, "n1", "small")
	n1 := newNode(t, caDir, "n1", 1).start()
	ready(t, n1)
	small := newNode(t, caDir, "small", 2, n1.addr()).start()
	ready(t, n1, small)

	n1Blobs, _ := blob.Open(t.TempDir())
	ServeBlobs(n1.n.RPC(), n1Blobs)
	client := NewClient(small.o.TLS)

	// Push a photo to n1, then fetch it back.
	full, thumb := []byte("full-size bytes"), []byte("thumb")
	hash := blob.Hash(full)
	if err := client.PutBlob(n1.addr(), hash, false, full); err != nil {
		t.Fatal(err)
	}
	if err := client.PutBlob(n1.addr(), hash, true, thumb); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetBlob(n1.addr(), hash, false)
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("GetBlob: %q, %v", got, err)
	}
	// A blob whose bytes don't match its name is refused.
	if err := client.PutBlob(n1.addr(), blob.Hash([]byte("other")), false, full); err == nil {
		t.Fatal("mismatched blob accepted")
	}

	// A group lives on n1 only (the small node isn't a voter or full).
	// A write for it made on the small node goes to n1.
	newGroup(n1, 100, "travato", 7)
	waitHolds(t, n1, 100, 7)
	waitFor(t, "the small node to know of the group", func() bool {
		g, _ := small.st.GroupByID(100)
		return g != nil
	})
	if small.n.Holds(100) {
		t.Fatal("the small node shouldn't hold the group")
	}
	small.apply(&cmd.CreatePost{GroupID: 100, PostID: 1, UserID: 7, Title: "via small", At: 1})
	if got := postTitles(t, n1.st, 100); len(got) != 1 {
		t.Fatalf("posts on n1: %v", got)
	}
	// Errors keep their identity across the network.
	_, err = small.n.Apply(&cmd.CreatePost{GroupID: 100, PostID: 2, UserID: 99, Title: "not a member", At: 1})
	if !errors.Is(err, cmd.ErrNotMember) || !cmd.IsInput(err) {
		t.Fatalf("forwarded error: %v", err)
	}

	if id, err := client.GroupID(n1.addr(), "travato"); err != nil || id != 100 {
		t.Fatalf("GroupID: %d, %v", id, err)
	}
	if _, err := client.GroupID(n1.addr(), "nope"); err == nil {
		t.Fatal("unknown group found")
	}
	if v := decodeValue([]byte("42")); v != int64(42) {
		t.Fatalf("decodeValue: %#v", v)
	}
}
