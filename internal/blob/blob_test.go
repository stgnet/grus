package blob

import (
	"os"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	used, err := s.Put([]byte("photo one"), []byte("thumb one"))
	if err != nil {
		t.Fatal(err)
	}
	unused, _ := s.Put([]byte("photo two"), []byte("thumb two"))
	if !s.Has(used) || !s.Has(unused) {
		t.Fatal("stored blobs missing")
	}
	if got, _ := s.Read(used, true); string(got) != "thumb one" {
		t.Fatalf("thumb read back as %q", got)
	}

	// Names are checked: no paths, no content that doesn't match.
	if _, err := s.Open("../../etc/passwd", false); err == nil {
		t.Fatal("opened a path outside the store")
	}
	if err := s.Write(Hash([]byte("x")), false, []byte("y")); err == nil {
		t.Fatal("wrote content under someone else's name")
	}

	// GC leaves new files alone, then removes old unused ones.
	keep := func(h string) bool { return h == used }
	if n, _ := s.GC(keep, time.Hour); n != 0 {
		t.Fatalf("GC removed %d fresh blobs", n)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, thumb := range []bool{false, true} {
		os.Chtimes(s.Path(unused, thumb), old, old)
		os.Chtimes(s.Path(used, thumb), old, old)
	}
	if n, _ := s.GC(keep, time.Hour); n != 1 {
		t.Fatalf("GC removed %d blobs, want 1", n)
	}
	if s.Has(unused) || !s.Has(used) {
		t.Fatal("GC removed the wrong blob")
	}
}
