package cmd

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stgnet/grus/internal/store"
)

func newStore(t *testing.T) *store.Store {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestReplaySkipsApplied: re-applying a log entry a file already has (what
// happens after a restart) changes nothing, and a failing command doesn't
// advance the index.
func TestReplaySkipsApplied(t *testing.T) {
	st := newStore(t)
	c := &CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: 1}
	if _, err := Run(st, 5, c); err != nil {
		t.Fatal(err)
	}
	// Same entry again: skipped, so no "slug taken" error.
	if _, err := Run(st, 5, c); err != nil {
		t.Fatalf("replay of an applied entry: %v", err)
	}
	// A new entry with the same slug is a real duplicate.
	_, err := Run(st, 6, &CreateGroup{GroupID: 2, Slug: "travato", Name: "Again", At: 1})
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("duplicate slug: %v", err)
	}
	if idx, _ := store.AppliedIndex(st.Site()); idx != 5 {
		t.Fatalf("applied index %d after a failed command, want 5", idx)
	}
}

// TestCreateGroupCompletesAfterCrash: if a node stopped after CreateGroup
// wrote site.db but before it wrote the group's file, replaying the entry
// finishes the job.
func TestCreateGroupCompletesAfterCrash(t *testing.T) {
	st := newStore(t)
	c := &CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: 1}
	// Simulate the half-applied state: only the site part.
	a := &Applier{Store: st, Index: 3}
	if err := a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO groups (id, slug, name, created_at) VALUES (1, 'travato', 'Travato', 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(st, 3, c); err != nil {
		t.Fatalf("replay: %v", err)
	}
	s, err := st.GroupSettings(1)
	if err != nil || s == nil || s.Name != "Travato" {
		t.Fatalf("group settings after replay: %+v, %v", s, err)
	}
}

func TestRedeemOnce(t *testing.T) {
	st := newStore(t)
	idx := uint64(0)
	run := func(c Command) (any, error) { idx++; return Run(st, idx, c) }

	if _, err := run(&CreateLogin{TokenHash: "t1", CodeHash: "c", Email: "a@x.com", ReturnURL: "/", At: 100, ExpiresAt: 200}); err != nil {
		t.Fatal(err)
	}
	v, err := run(&RedeemLogin{TokenHash: "t1", NewUserID: 7, SessionHash: "s1", SessionExpires: 999, At: 150})
	if err != nil {
		t.Fatal(err)
	}
	if v.(Redeemed).UserID != 7 {
		t.Fatalf("new account id %d, want 7", v.(Redeemed).UserID)
	}
	// Link and code racing: the second redemption loses.
	if _, err := run(&RedeemLogin{TokenHash: "t1", NewUserID: 8, SessionHash: "s2", SessionExpires: 999, At: 151}); !errors.Is(err, ErrLoginDead) {
		t.Fatalf("second redemption: %v", err)
	}
	// Expired.
	run(&CreateLogin{TokenHash: "t2", CodeHash: "c", Email: "a@x.com", ReturnURL: "/", At: 100, ExpiresAt: 200})
	if _, err := run(&RedeemLogin{TokenHash: "t2", NewUserID: 9, SessionHash: "s3", SessionExpires: 999, At: 200}); !errors.Is(err, ErrLoginDead) {
		t.Fatalf("expired redemption: %v", err)
	}
	// A second sign-in by the same email finds the same account.
	run(&CreateLogin{TokenHash: "t3", CodeHash: "c", Email: "a@x.com", ReturnURL: "/", At: 300, ExpiresAt: 400})
	v, err = run(&RedeemLogin{TokenHash: "t3", NewUserID: 10, SessionHash: "s4", SessionExpires: 999, At: 301})
	if err != nil || v.(Redeemed).UserID != 7 {
		t.Fatalf("returning sign-in: %v, %v", v, err)
	}
}

func TestPurge(t *testing.T) {
	st := newStore(t)
	idx := uint64(0)
	run := func(c Command) {
		idx++
		if _, err := Run(st, idx, c); err != nil {
			t.Fatal(err)
		}
	}
	run(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: 1})

	db, _ := st.Group(1)
	exec := func(q string) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Post 1: deleted, past purge. Post 2: deleted, past purge, but on legal
	// hold. Post 3: live. Each has a comment and a revision.
	exec(`INSERT INTO posts (id, title, created_at, last_activity_at, status, purge_after, legal_hold) VALUES
		(1, 'gone', 1, 1, 'deleted', 50, 0), (2, 'held', 1, 1, 'deleted', 50, 1), (3, 'live', 1, 1, 'visible', NULL, 0)`)
	exec(`INSERT INTO comments (id, post_id, body, created_at) VALUES (10, 1, 'a', 1), (11, 2, 'b', 1), (12, 3, 'c', 1)`)
	exec(`INSERT INTO revisions (kind, ref_id, version, body, edited_by, edited_at, purge_after) VALUES
		('post', 1, 1, 'x', 1, 1, 50), ('post', 2, 1, 'x', 1, 1, 50), ('post', 3, 1, 'x', 1, 1, 50), ('post', 3, 2, 'y', 1, 1, 500)`)

	run(&Purge{Before: 100})

	count := func(q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(`SELECT COUNT(*) FROM posts`); got != 2 {
		t.Errorf("%d posts left, want 2 (held + live)", got)
	}
	if got := count(`SELECT COUNT(*) FROM comments`); got != 2 {
		t.Errorf("%d comments left, want 2 (on held + live posts)", got)
	}
	// Held post's revision stays; live post's expired revision goes; its
	// unexpired one stays.
	if got := count(`SELECT COUNT(*) FROM revisions`); got != 2 {
		t.Errorf("%d revisions left, want 2", got)
	}
}
