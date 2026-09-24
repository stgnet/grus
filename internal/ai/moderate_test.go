package ai

import (
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cmd"
)

// The check hides clear violations and flags borderline items, with the
// reason in the mod log under no actor; a mod's approval becomes an
// example that the next check reads, and the AI doesn't flag that item
// again.
func TestModerationCheck(t *testing.T) {
	e := newEnv(t)
	at := e.now.Unix()
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 100, UserID: 5, Title: "BUY CHEAP pills", Body: "Click here.", At: at})
	e.apply(&cmd.CreatePost{GroupID: 1, PostID: 200, UserID: 5, Title: "Fridge fan rattle", Body: "Rattles.", At: at})
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 201, PostID: 200, UserID: 5, Body: "Replace the fan, you idiot.", At: at})
	e.now = e.now.Add(time.Minute)
	e.drain()

	if n := e.count(`SELECT COUNT(*) FROM posts WHERE id = 100 AND status = 'auto_hidden' AND flagged_by = 'auto' AND flag_category = 'spam'`); n != 1 {
		t.Fatal("spam not hidden")
	}
	if n := e.count(`SELECT COUNT(*) FROM search_fts WHERE rowid = 100`); n != 0 {
		t.Fatal("hidden post still searchable")
	}
	if n := e.count(`SELECT COUNT(*) FROM comments WHERE id = 201 AND status = 'flagged' AND flag_reason = 'Calls another member an idiot.'`); n != 1 {
		t.Fatal("borderline comment not flagged")
	}
	if n := e.count(`SELECT COUNT(*) FROM mod_log WHERE actor_id IS NULL AND action IN ('auto_hide', 'flag')`); n != 2 {
		t.Fatal("AI actions not in the mod log as automatic")
	}
	if n := e.count(`SELECT COUNT(*) FROM posts WHERE id = 200 AND status = 'visible'`); n != 1 {
		t.Fatal("a clear post was touched")
	}

	// A mod keeps the comment: it's cleared for good and becomes an example.
	e.apply(&cmd.Approve{GroupID: 1, Kind: "comment", ID: 201, By: 5, At: e.now.Unix()})
	e.apply(&cmd.EditComment{GroupID: 1, CommentID: 201, EditorID: 5, Body: "Replace the fan, you idiot. (Kidding.)", At: e.now.Unix()})
	e.now = e.now.Add(2 * time.Hour)
	e.drain()
	if n := e.count(`SELECT COUNT(*) FROM comments WHERE id = 201 AND status = 'visible' AND ai_cleared = 1`); n != 1 {
		t.Fatal("a mod-approved comment was flagged again")
	}
	e.apply(&cmd.CreateComment{GroupID: 1, CommentID: 202, PostID: 200, UserID: 5, Body: "Same here.", At: e.now.Unix()})
	e.now = e.now.Add(time.Minute)
	e.drain()
	last := e.llm.prompts[len(e.llm.prompts)-1]
	for _, p := range e.llm.prompts {
		if strings.Contains(p, "ITEM:\nSame here.") {
			last = p
		}
	}
	if !strings.Contains(last, "- kept up: Replace the fan, you idiot.") || !strings.Contains(last, "THE POST THIS COMMENT IS ON:") {
		t.Fatalf("check prompt without the example or context:\n%s", last)
	}
}
