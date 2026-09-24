package cmd

import (
	"errors"
	"testing"
)

func TestReportsVotesAndBans(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	run := d.Apply
	must := func(c Command) any {
		t.Helper()
		v, err := run(c)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const day = 86400
	must(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", OwnerID: 1, At: 0})
	must(&UpdateSettings{GroupID: 1, Set: map[string]any{"vote_threshold": 2}, By: 1, At: 0})
	// Members 2..5 joined long ago and have posted; 6 is new.
	for u := int64(2); u <= 6; u++ {
		at := int64(0)
		if u == 6 {
			at = 40 * day
		}
		must(&JoinGroup{GroupID: 1, UserID: u, At: at})
		must(&CreatePost{GroupID: 1, PostID: 1000 + u, UserID: u, Title: "Hello", At: at})
	}
	now := int64(41 * day)
	must(&CreatePost{GroupID: 1, PostID: 100, UserID: 2, Title: "Thread", At: now})
	must(&CreateComment{GroupID: 1, CommentID: 101, PostID: 100, UserID: 3, Body: "Rude thing", At: now})
	must(&CreateComment{GroupID: 1, CommentID: 102, PostID: 100, ParentID: 101, UserID: 4, Body: "Hey, no", At: now})

	// A report flags it and queues a fresh check.
	if _, err := run(&Report{GroupID: 1, Kind: "comment", ID: 101, UserID: 3, At: now}); !IsInput(err) {
		t.Fatal("reported own comment")
	}
	must(&Report{GroupID: 1, Kind: "comment", ID: 101, UserID: 2, Reason: "insult", At: now})
	db, _ := st.Group(1)
	var status, by string
	db.QueryRow(`SELECT status, flagged_by FROM comments WHERE id = 101`).Scan(&status, &by)
	if status != "flagged" || by != "report" {
		t.Fatalf("after report: %s %s", status, by)
	}
	// Who can vote: not the author (3), not the one arguing (4, who
	// replied), not a new member (6).
	for _, u := range []int64{3, 4, 6} {
		if _, err := run(&Vote{GroupID: 1, Kind: "comment", ID: 101, UserID: u, Vote: "hide", At: now}); !IsInput(err) {
			t.Fatalf("user %d voted: %v", u, err)
		}
	}
	must(&Vote{GroupID: 1, Kind: "comment", ID: 101, UserID: 2, Vote: "hide", At: now})
	if v := must(&Vote{GroupID: 1, Kind: "comment", ID: 101, UserID: 5, Vote: "hide", At: now}); v != "hidden" {
		t.Fatalf("vote result %v", v)
	}
	var count int
	db.QueryRow(`SELECT status FROM comments WHERE id = 101`).Scan(&status)
	db.QueryRow(`SELECT comment_count FROM posts WHERE id = 100`).Scan(&count)
	if status != "auto_hidden" || count != 1 {
		t.Fatalf("after hide votes: %s, count %d", status, count)
	}
	// A mod approves it after all: visible, counted, cleared, report closed.
	must(&Approve{GroupID: 1, Kind: "comment", ID: 101, By: 1, At: now})
	var open int
	db.QueryRow(`SELECT COUNT(*) FROM reports WHERE resolved_at IS NULL`).Scan(&open)
	db.QueryRow(`SELECT comment_count FROM posts WHERE id = 100`).Scan(&count)
	if open != 0 || count != 2 {
		t.Fatalf("after approve: %d open reports, count %d", open, count)
	}
	// Cleared items aren't flagged by a new report.
	must(&Report{GroupID: 1, Kind: "comment", ID: 101, UserID: 5, At: now})
	db.QueryRow(`SELECT status FROM comments WHERE id = 101`).Scan(&status)
	if status != "visible" {
		t.Fatal("a cleared item was flagged again")
	}

	// Roles and bans.
	must(&SetRole{GroupID: 1, UserID: 2, Role: "mod", By: 1, At: now})
	if _, err := run(&SetRole{GroupID: 1, UserID: 1, Role: "member", By: 1, At: now}); !IsInput(err) {
		t.Fatal("demoted the only owner")
	}
	if _, err := run(&Ban{GroupID: 1, UserID: 1, By: 2, At: now}); !IsInput(err) {
		t.Fatal("banned an owner")
	}
	must(&SetRole{GroupID: 1, UserID: 5, Role: "mod", By: 1, At: now})
	if _, err := run(&Ban{GroupID: 1, UserID: 5, By: 2, At: now}); !IsInput(err) {
		t.Fatal("a mod banned a mod")
	}
	must(&Ban{GroupID: 1, UserID: 3, Until: now + day, Reason: "insults", By: 2, At: now})
	if _, err := run(&CreateComment{GroupID: 1, CommentID: 103, PostID: 100, UserID: 3, Body: "again", At: now}); !errors.Is(err, ErrNotMember) {
		t.Fatalf("banned member commented: %v", err)
	}
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 3, At: now + 10}); !IsInput(err) {
		t.Fatal("rejoined during a ban")
	}
	if v := must(&JoinGroup{GroupID: 1, UserID: 3, At: now + 2*day}); v != "active" {
		t.Fatalf("rejoin after the ban ran out: %v", v)
	}
	// Banning a non-member keeps them out.
	must(&Ban{GroupID: 1, UserID: 99, By: 1, At: now})
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 99, At: now}); !IsInput(err) {
		t.Fatal("banned non-member joined")
	}
	must(&Unban{GroupID: 1, UserID: 99, By: 1, At: now})
	must(&JoinGroup{GroupID: 1, UserID: 99, At: now})

	// Lock and pin.
	must(&SetPostFlag{GroupID: 1, PostID: 100, Flag: "locked", On: true, By: 2, At: now})
	if _, err := run(&CreateComment{GroupID: 1, CommentID: 104, PostID: 100, UserID: 5, Body: "late", At: now}); !errors.Is(err, ErrLocked) {
		t.Fatalf("comment on a locked thread: %v", err)
	}
}

func TestHoldFirstPost(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	run := d.Apply
	run(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", OwnerID: 1, At: 0})
	run(&UpdateSettings{GroupID: 1, Set: map[string]any{"hold_first_post": true}, By: 1, At: 0})
	run(&JoinGroup{GroupID: 1, UserID: 2, At: 0})
	run(&CreatePost{GroupID: 1, PostID: 100, UserID: 2, Title: "First", At: 1})
	run(&CreatePost{GroupID: 1, PostID: 101, UserID: 1, Title: "Owner's", At: 1})
	p, _ := st.Post(1, 100)
	o, _ := st.Post(1, 101)
	if p.Status != "held" || o.Status != "visible" {
		t.Fatalf("statuses %s %s", p.Status, o.Status)
	}
	// Still held for the second post, until a mod approves the first.
	run(&CreatePost{GroupID: 1, PostID: 102, UserID: 2, Title: "Second", At: 2})
	if p, _ := st.Post(1, 102); p.Status != "held" {
		t.Fatal("second post not held")
	}
	if _, err := run(&Approve{GroupID: 1, Kind: "post", ID: 100, By: 1, At: 3}); err != nil {
		t.Fatal(err)
	}
	run(&CreatePost{GroupID: 1, PostID: 103, UserID: 2, Title: "Third", At: 4})
	if p, _ := st.Post(1, 103); p.Status != "visible" {
		t.Fatal("post after an approved one held")
	}
}
