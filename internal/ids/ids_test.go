package ids

import (
	"testing"
	"time"
)

func TestIncreasingAndUnique(t *testing.T) {
	g := New(3)
	fixed := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return fixed } // every id in the same millisecond

	seen := map[int64]bool{}
	var last int64
	for i := 0; i < 10000; i++ { // more than 4096, so the sequence overflows
		id := g.Next()
		if id <= last {
			t.Fatalf("id %d not greater than previous %d", id, last)
		}
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
		last = id
	}
}

func TestClockGoingBackwards(t *testing.T) {
	g := New(1)
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	a := g.Next()
	now = now.Add(-time.Second)
	if b := g.Next(); b <= a {
		t.Fatalf("id went backwards with the clock: %d then %d", a, b)
	}
}

func TestNodesDiffer(t *testing.T) {
	fixed := func() time.Time { return time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC) }
	a, b := New(1), New(2)
	a.now, b.now = fixed, fixed
	if a.Next() == b.Next() {
		t.Fatal("two nodes produced the same id in the same millisecond")
	}
}
