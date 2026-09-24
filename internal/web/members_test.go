package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stgnet/grus/internal/cmd"
)

// makeOwner makes a signed-in account an owner of the test group.
func (s *testSite) makeOwner(handle string) {
	s.t.Helper()
	s.makeMod(handle)
	db, _ := s.st.Group(42)
	var id int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = ?`, handle).Scan(&id)
	db.Exec(`UPDATE memberships SET role = 'owner' WHERE user_id = ?`, id)
}

func TestJoinApprovalAndInvites(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	owner := s.signedIn("owner@example.com", "owner")
	s.makeOwner("owner")
	expect(t, owner.do("POST", G+"/settings", url.Values{"visibility": {"private"}, "join_policy": {"approval"},
		"join_questions": {"Do you own a Travato?"}}), 303, "/settings?saved=1")
	owner.upload(G+"/submit", map[string]string{"title": "Members-only tips", "body": "Secret."}, nil)

	// A signed-in non-member sees the name and "Ask to join", not posts.
	bob := s.signedIn("bob@example.com", "bob")
	front := bob.do("GET", G+"/", nil).Body.String()
	if !strings.Contains(front, "Ask to join") || strings.Contains(front, "Members-only tips") {
		t.Fatalf("private front page:\n%s", front)
	}
	if !strings.Contains(bob.do("GET", G+"/join", nil).Body.String(), "Do you own a Travato?") {
		t.Fatal("join questions not shown")
	}
	w := bob.do("POST", G+"/join", url.Values{"answers": {"Yes, a 2021 59K"}})
	if !strings.Contains(w.Body.String(), "request is with the group") {
		t.Fatalf("after asking to join:\n%s", w.Body.String())
	}
	// The owner sees the request with its answers and lets bob in.
	mm := owner.do("GET", G+"/mod/members", nil).Body.String()
	if !strings.Contains(mm, "bob") || !strings.Contains(mm, "2021 59K") {
		t.Fatalf("members page:\n%s", mm)
	}
	expect(t, bob.do("GET", G+"/mod/members", nil), 404, "")
	var bobID int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = 'bob'`).Scan(&bobID)
	expect(t, owner.do("POST", G+"/mod/members/approve", url.Values{"user": {fmt.Sprint(bobID)}}), 303, "/mod/members")
	if !strings.Contains(bob.do("GET", G+"/", nil).Body.String(), "Members-only tips") {
		t.Fatal("approved member can't read")
	}

	// Hidden and invite-only: invisible without a code, open with one.
	expect(t, owner.do("POST", G+"/settings", url.Values{"visibility": {"hidden"}, "join_policy": {"invite"}}), 303, "/settings?saved=1")
	w = owner.do("POST", G+"/mod/members/invite", url.Values{"uses": {"10"}, "days": {"7"}})
	loc := w.Header().Get("Location")
	code := strings.TrimPrefix(loc, "/mod/members?new=")
	if code == loc || len(code) != 16 {
		t.Fatalf("invite redirect %q", loc)
	}
	if !strings.Contains(owner.do("GET", G+loc, nil).Body.String(), G+"/invite/"+code) {
		t.Fatal("new invite link not shown")
	}
	carol := s.signedIn("carol@example.com", "carol")
	expect(t, carol.do("GET", G+"/", nil), 404, "")
	expect(t, carol.do("GET", G+"/invite/nosuchcode1234", nil), 404, "")
	if !strings.Contains(carol.do("GET", G+"/invite/"+code, nil).Body.String(), "Join Travato Owners") {
		t.Fatal("invite page")
	}
	// Signed out, the invite page offers sign-in that comes back to it.
	if !strings.Contains(s.browser().do("GET", G+"/invite/"+code, nil).Body.String(), "Sign in to join") {
		t.Fatal("invite page for a signed-out reader")
	}
	expect(t, carol.do("POST", G+"/invite/"+code, url.Values{}), 303, "/")
	expect(t, carol.do("GET", G+"/", nil), 200, "")
	// Revoked, it's a 404 like any bad code.
	expect(t, owner.do("POST", G+"/mod/members/revoke", url.Values{"code": {code}}), 303, "/mod/members")
	expect(t, s.signedIn("dave@example.com", "dave").do("GET", G+"/invite/"+code, nil), 404, "")
}

func TestAnonymousPosting(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	owner := s.signedIn("owner@example.com", "owner")
	s.makeOwner("owner")
	alice := s.signedIn("alice@example.com", "alice")

	// Not offered, and refused, until the group allows it.
	if strings.Contains(alice.do("GET", G+"/submit", nil).Body.String(), "Post anonymously") {
		t.Fatal("anonymous box offered in a group that doesn't allow it")
	}
	w := alice.upload(G+"/submit", map[string]string{"title": "Embarrassing question", "anonymous": "on"}, nil)
	expect(t, w, 400, "")
	expect(t, owner.do("POST", G+"/settings", url.Values{"allow_anonymous": {"on"}}), 303, "/settings?saved=1")
	w = alice.upload(G+"/submit", map[string]string{"title": "Embarrassing question", "anonymous": "on"}, nil)
	expect(t, w, 303, "")
	post := w.Header().Get("Location")

	if page := s.browser().do("GET", G+post, nil).Body.String(); !strings.Contains(page, "Anonymous member") || strings.Contains(page, "alice") {
		t.Fatalf("anonymous post shows its author:\n%s", page)
	}
	if !strings.Contains(alice.do("GET", G+post, nil).Body.String(), "Anonymous member (you)") {
		t.Fatal("the author doesn't see it's theirs")
	}
	// A mod can find out, with a reason, and it's logged.
	mod := owner
	if !strings.Contains(mod.do("GET", G+post, nil).Body.String(), "Who wrote this?") {
		t.Fatal("no reveal link for mods")
	}
	expect(t, alice.do("GET", G+post+"/reveal", nil), 404, "")
	expect(t, mod.do("POST", G+post+"/reveal", url.Values{"reason": {""}}), 400, "")
	page := mod.do("POST", G+post+"/reveal", url.Values{"reason": {"Report of spam"}}).Body.String()
	if !strings.Contains(page, "<strong>alice</strong>") {
		t.Fatalf("reveal:\n%s", page)
	}
	db, _ := s.st.Group(42)
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM mod_log WHERE action = 'reveal_author' AND reason = 'Report of spam'`).Scan(&n)
	if n != 1 {
		t.Fatal("reveal not logged")
	}
}

// A note written from a sister group's post shows with the group's name
// and a link to its address, and disappears when the visibility rule
// stops allowing it.
func TestSisterNoteOnPost(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	alice := s.signedIn("alice@example.com", "alice")
	must(t, s.log, &cmd.CreateGroup{GroupID: 43, Slug: "promaster", Name: "ProMaster", At: 1})
	must(t, s.log, &cmd.ProposeSister{GroupID: 42, Other: 43, By: 1, At: 2})
	must(t, s.log, &cmd.AnswerSister{GroupID: 43, Other: 42, Accept: true, By: 1, At: 2})
	var aliceID int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = 'alice'`).Scan(&aliceID)
	must(t, s.log, &cmd.JoinGroup{GroupID: 43, UserID: aliceID, At: 2})
	must(t, s.log, &cmd.CreatePost{GroupID: 43, PostID: 500, UserID: aliceID, Title: "Death wobble fix", At: 3})
	post := alice.upload(G+"/submit", map[string]string{"title": "Wobble at 60"}, nil).Header().Get("Location")
	var id int64
	fmt.Sscanf(post, "/p/%d", &id)
	// What the check job and note job would have written.
	db, _ := s.st.Group(42)
	db.Exec(`INSERT INTO sister_links (post_id, other_group, other_post, source, other_title, other_date, created_at)
		VALUES (?, 43, 500, 'auto', 'Death wobble fix', 3, 3)`, id)
	db.Exec(`INSERT INTO notes (id, host_post_id, kind, text, stale, created_at, updated_at) VALUES (9, ?, 'link', 'Replacing the steering damper fixed it.', 0, 3, 3)`, id)
	db.Exec(`INSERT INTO note_sources (note_id, group_id, post_id) VALUES (9, 43, 500)`)

	page := s.browser().do("GET", G+post, nil).Body.String()
	if !strings.Contains(page, "In the ProMaster group") || !strings.Contains(page, "https://promaster.nfb.group/p/500") {
		t.Fatalf("sister note:\n%s", page)
	}
	// The ProMaster group goes private: its content can't be shown here.
	must(t, s.log, &cmd.UpdateSettings{GroupID: 43, Set: map[string]any{"visibility": "private"}, By: 1, At: 4})
	if strings.Contains(s.browser().do("GET", G+post, nil).Body.String(), "steering damper") {
		t.Fatal("note from a private group shown")
	}
	// The About page still lists it: a private group's name is public.
	if !strings.Contains(s.browser().do("GET", G+"/about", nil).Body.String(), "ProMaster") {
		t.Fatal("sister not on the About page")
	}
}
