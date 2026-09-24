package ai

import (
	"context"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cmd"
)

// Sister groups: a new post's check finds a matching post in the sister
// group, and each side gets a note only where the visibility rule allows.
// An edit on the other side reaches the note through the sweep, and
// deleting the other post takes the note down.
func TestSisterNotes(t *testing.T) {
	// The note on Travato post 200 written from ProMaster post 500.
	const sisterNote = `SELECT COUNT(*) FROM notes n JOIN note_sources s ON s.note_id = n.id
		WHERE n.host_post_id = 200 AND s.group_id = 2 AND s.post_id = 500`
	e := newEnv(t)
	at := e.now.Unix()
	// Group 2: the ProMaster group, public. Group 1 (Travato) goes private.
	e.apply(&cmd.CreateGroup{GroupID: 2, Slug: "promaster", Name: "ProMaster", At: at})
	e.apply(&cmd.JoinGroup{GroupID: 2, UserID: 5, At: at})
	e.apply(&cmd.UpdateSettings{GroupID: 1, Set: map[string]any{"visibility": "private"}, By: 5, At: at})
	e.apply(&cmd.CreatePost{GroupID: 2, PostID: 500, UserID: 5, Title: "Death wobble after tire change", Body: "Steering damper fixed it.", At: at})
	e.now = e.now.Add(time.Hour)
	e.drain()

	// Not paired yet: no cross-links.
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "Death wobble on the highway", Body: "Wobble after new tires.", At: e.now.Unix()})
	e.now = e.now.Add(time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM sister_links`); n != 0 {
		t.Fatal("linked to a group that isn't a sister")
	}

	// Pair them: proposed by one owner, accepted by the other.
	if v := e.apply(&cmd.ProposeSister{GroupID: 1, Other: 2, Topics: "chassis , wobble", By: 5, At: e.now.Unix()}); v != "proposed" {
		t.Fatalf("propose: %v", v)
	}
	e.apply(&cmd.AnswerSister{GroupID: 2, Other: 1, Accept: true, By: 5, At: e.now.Unix()})

	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 200, UserID: 5, Title: "Death wobble again", Body: "Wobble at 60.", At: e.now.Unix()})
	e.now = e.now.Add(time.Hour)
	e.drain()
	// The private Travato post gets a note from the public ProMaster post;
	// the ProMaster post gets none (it would summarize private content).
	if n := e.count(`SELECT COUNT(*) FROM notes n JOIN note_sources s ON s.note_id = n.id
		WHERE n.host_post_id = 200 AND s.group_id = 2 AND s.post_id = 500 AND n.text != ''`); n != 1 {
		t.Fatal("no sister note on the private group's post")
	}
	db2, _ := e.st.Group(2)
	var n2 int
	db2.QueryRow(`SELECT COUNT(*) FROM notes WHERE host_post_id = 500`).Scan(&n2)
	if n2 != 0 {
		t.Fatal("a public group's post got a note written from a private group's post")
	}
	db2.QueryRow(`SELECT COUNT(*) FROM sister_links WHERE post_id = 500 AND other_group = 1 AND other_post = 200`).Scan(&n2)
	if n2 != 1 {
		t.Fatal("the sister group doesn't have its side of the link")
	}

	// The ProMaster thread gets a comment: the sweep marks the note stale
	// and it's rewritten (the fake says "no change", which keeps the text).
	e.apply(&cmd.CreateComment{GroupID: 2, CommentID: 501, PostID: 500, UserID: 5, Body: "Also check the track bar.", At: e.now.Unix()})
	if err := e.w.sweepSisters(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.count(sisterNote + ` AND n.stale = 1 AND n.ext_version = 2`); n != 1 {
		t.Fatal("sweep didn't mark the note stale")
	}
	e.now = e.now.Add(2 * time.Hour) // notes are rewritten at most hourly
	e.drain()
	if n := e.count(sisterNote + ` AND n.stale = 0`); n != 1 {
		t.Fatal("stale sister note not rewritten")
	}

	// Deleting the ProMaster post takes the note down; restoring brings it back.
	e.apply(&cmd.SoftDelete{GroupID: 2, Kind: "post", ID: 500, By: 5, At: e.now.Unix()})
	if n := e.count(sisterNote + ` AND n.state = 'active'`); n != 0 {
		t.Fatal("note from a deleted post still showing")
	}
	e.apply(&cmd.Restore{GroupID: 2, Kind: "post", ID: 500, By: 5, At: e.now.Unix()})
	if n := e.count(sisterNote + ` AND n.state = 'active'`); n != 1 {
		t.Fatal("restored post's note didn't come back")
	}

	// A mod removes the cross-link: rejected on both sides, never relinked.
	e.apply(&cmd.RemoveSisterLink{GroupID: 2, PostID: 500, OtherGroup: 1, OtherPost: 200, By: 5, At: e.now.Unix()})
	if n := e.count(sisterNote + ` AND n.state = 'active'`); n != 0 {
		t.Fatal("removed cross-link's note still showing")
	}
	if n := e.count(`SELECT COUNT(*) FROM sister_links WHERE post_id = 200 AND state = 'rejected'`); n != 1 {
		t.Fatal("rejection not recorded on the other side")
	}

	// Ending the pairing clears active links; a new post isn't linked.
	e.apply(&cmd.EndSister{GroupID: 1, Other: 2, By: 5, At: e.now.Unix()})
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 300, UserID: 5, Title: "Wobble wobble", Body: "Death wobble.", At: e.now.Unix()})
	e.now = e.now.Add(time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM sister_links WHERE post_id = 300`); n != 0 {
		t.Fatal("linked after the pairing ended")
	}
}
