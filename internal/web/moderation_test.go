package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stgnet/grus/internal/cmd"
)

// join makes a signed-in account a long-standing member (joined at time 1),
// so it may vote.
func (s *testSite) join(handle string) int64 {
	s.t.Helper()
	var id int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = ?`, handle).Scan(&id)
	must(s.t, s.log, &cmd.JoinGroup{GroupID: 42, UserID: id, At: 1})
	return id
}

func TestNewAccountLimits(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	newbie := s.newcomer("new@example.com", "newbie")

	// No links, in a post or a comment.
	w := newbie.upload(G+"/submit", map[string]string{"title": "Cheap parts", "body": "See www.example.com"}, nil)
	expect(t, w, 400, "")
	if !strings.Contains(w.Body.String(), "can&#39;t post links yet") {
		t.Fatalf("no link message:\n%s", w.Body.String())
	}
	// A few posts an hour, then a pause.
	for i := range 5 {
		expect(t, newbie.upload(G+"/submit", map[string]string{"title": fmt.Sprintf("Question %d", i)}, nil), 303, "")
	}
	w = newbie.upload(G+"/submit", map[string]string{"title": "One too many"}, nil)
	expect(t, w, 400, "")
	if !strings.Contains(w.Body.String(), "a few times an hour") {
		t.Fatalf("no rate message:\n%s", w.Body.String())
	}

	// An established member isn't limited.
	alice := s.signedIn("alice@example.com", "alice")
	expect(t, alice.upload(G+"/submit", map[string]string{"title": "Hello"}, nil), 303, "")
	expect(t, alice.upload(G+"/submit", map[string]string{"title": "Fan", "body": "https://example.com/fan"}, nil), 303, "")
}

func TestReportQueueAndVotes(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	alice := s.signedIn("alice@example.com", "alice")
	bob := s.signedIn("bob@example.com", "bob")
	carol := s.signedIn("carol@example.com", "carol")
	mod := s.signedIn("mod@example.com", "mod")
	s.makeMod("mod")
	s.join("bob")
	s.join("carol")

	loc := alice.upload(G+"/submit", map[string]string{"title": "Rude post", "body": "Nobody asked you."}, nil).Header().Get("Location")
	postURL := G + loc
	// bob and carol have something shown in the group, so they may vote.
	bob.upload(postURL+"/comment", map[string]string{"body": "Hm."}, nil)
	carol.upload(G+"/submit", map[string]string{"title": "Tires"}, nil)

	// bob reports it: it's flagged for everyone, and members can vote.
	if !strings.Contains(bob.do("GET", postURL, nil).Body.String(), loc+"/report") {
		t.Fatal("no Report link for a member")
	}
	expect(t, bob.do("POST", postURL+"/report", url.Values{"reason": {"insulting"}}), 200, "")
	page := carol.do("GET", postURL, nil).Body.String()
	if !strings.Contains(page, "Flagged as possibly breaking") || !strings.Contains(page, `value="hide"`) {
		t.Fatalf("flagged post page:\n%s", page)
	}
	expect(t, carol.do("POST", postURL+"/vote", url.Values{"vote": {"hide"}}), 303, loc)
	if !strings.Contains(carol.do("GET", postURL, nil).Body.String(), "Hide (1)") {
		t.Fatal("vote not counted")
	}
	// The author can't vote on their own post.
	expect(t, alice.do("POST", postURL+"/vote", url.Values{"vote": {"keep"}}), 400, "")

	// The mod sees it in the queue with the report's reason, and lets it stand.
	q := mod.do("GET", G+"/mod/queue", nil).Body.String()
	if !strings.Contains(q, "Rude post") || !strings.Contains(q, "insulting") {
		t.Fatalf("mod queue:\n%s", q)
	}
	if !strings.Contains(mod.do("GET", G+"/", nil).Body.String(), "Queue (1)") {
		t.Fatal("no queue count on the feed tabs")
	}
	expect(t, mod.do("POST", G+"/mod/queue/approve", url.Values{"kind": {"post"}, "id": {idOf(loc)}}), 303, "/mod/queue")
	if strings.Contains(mod.do("GET", G+"/mod/queue", nil).Body.String(), "Rude post") {
		t.Fatal("still queued after approval")
	}
	if strings.Contains(carol.do("GET", postURL, nil).Body.String(), "Flagged as possibly") {
		t.Fatal("still flagged after approval")
	}
	// A report after a mod cleared it doesn't flag it again.
	expect(t, carol.do("POST", postURL+"/report", url.Values{"reason": {"still rude"}}), 200, "")
	if strings.Contains(bob.do("GET", postURL, nil).Body.String(), "Flagged as possibly") {
		t.Fatal("re-flagged after a mod cleared it")
	}

	// Members can't see the queue or the log.
	expect(t, bob.do("GET", G+"/mod/queue", nil), 404, "")
	expect(t, bob.do("GET", G+"/mod/log", nil), 404, "")

	// Remove with a reason: shown in its place, and in the log.
	expect(t, mod.do("POST", G+"/mod/queue/remove", url.Values{"kind": {"post"}, "id": {idOf(loc)}, "reason": {"Rule 2: be kind"}}), 303, "/mod/queue")
	if !strings.Contains(mod.do("GET", postURL, nil).Body.String(), "Removed by a moderator: Rule 2: be kind") {
		t.Fatal("removal reason not shown")
	}
	if !strings.Contains(alice.do("GET", "https://nfb.group/notifications", nil).Body.String(), "A moderator removed your post in “Rude post”") {
		t.Fatal("author not told of the removal")
	}
	lg := mod.do("GET", G+"/mod/log", nil).Body.String()
	if !strings.Contains(lg, "Rule 2: be kind") || !strings.Contains(lg, "approve") {
		t.Fatalf("mod log:\n%s", lg)
	}
}

func TestLockPinBanSuspend(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	owner := s.signedIn("owner@example.com", "owner")
	s.makeOwner("owner")
	bob := s.signedIn("bob@example.com", "bob")
	loc := owner.upload(G+"/submit", map[string]string{"title": "Rules"}, nil).Header().Get("Location")
	postURL := G + loc

	// Lock: no more comments. Pin: top of the feed.
	expect(t, owner.do("POST", postURL+"/lock", url.Values{"on": {"1"}}), 303, loc)
	expect(t, bob.upload(postURL+"/comment", map[string]string{"body": "late"}, nil), 400, "")
	expect(t, owner.do("POST", postURL+"/pin", url.Values{"on": {"1"}}), 303, loc)
	bob.upload(G+"/submit", map[string]string{"title": "Newer post"}, nil)
	feed := s.browser().do("GET", G+"/?sort=new", nil).Body.String()
	if strings.Index(feed, "Rules") > strings.Index(feed, "Newer post") {
		t.Fatal("pinned post isn't first")
	}
	// Members can't lock.
	expect(t, bob.do("POST", postURL+"/lock", url.Values{"on": {"0"}}), 404, "")

	// Ban bob by handle: he can't post; unban and he can.
	expect(t, owner.do("POST", G+"/mod/member/ban", url.Values{"handle": {"bob"}, "days": {"7"}, "reason": {"spam"}}), 303, "/mod/members#members")
	if !strings.Contains(owner.do("GET", G+"/mod/members", nil).Body.String(), "banned until") {
		t.Fatal("ban not listed")
	}
	w := bob.upload(G+"/submit", map[string]string{"title": "Back again"}, nil)
	if w.Code == 303 {
		t.Fatal("banned member could post")
	}
	var bobID int64
	s.st.Site().QueryRow(`SELECT id FROM users WHERE handle = 'bob'`).Scan(&bobID)
	expect(t, owner.do("POST", G+"/mod/member/unban", url.Values{"user": {fmt.Sprint(bobID)}}), 303, "")
	expect(t, bob.upload(G+"/submit", map[string]string{"title": "Back again"}, nil), 303, "")
	// Owners can't be banned.
	expect(t, owner.do("POST", G+"/mod/member/ban", url.Values{"handle": {"owner"}, "days": {"7"}}), 400, "")

	// The owner makes bob a mod; now he sees the queue.
	expect(t, owner.do("POST", G+"/mod/member/role", url.Values{"user": {fmt.Sprint(bobID)}, "role": {"mod"}}), 303, "")
	expect(t, bob.do("GET", G+"/mod/queue", nil), 200, "")

	// The operator suspends bob site-wide: he reads as signed out.
	op := s.signedIn("scott@example.com", "scott")
	expect(t, op.do("POST", "https://nfb.group/admin/suspend", url.Values{"who": {"bob"}, "days": {"7"}}), 303, "/admin")
	if strings.Contains(bob.do("GET", G+"/", nil).Body.String(), `profile">bob<`) {
		t.Fatal("suspended account still signed in")
	}
}

func TestHeldFirstPostPage(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	owner := s.signedIn("owner@example.com", "owner")
	s.makeOwner("owner")
	expect(t, owner.do("POST", G+"/settings", url.Values{"hold_first_post": {"on"}}), 303, "/settings?saved=1")

	bob := s.signedIn("bob@example.com", "bob")
	loc := bob.upload(G+"/submit", map[string]string{"title": "First post"}, nil).Header().Get("Location")
	// bob sees it with a note; others don't see it at all until it's let through.
	if !strings.Contains(bob.do("GET", G+loc, nil).Body.String(), "Only you and the moderators") {
		t.Fatal("no held note for the author")
	}
	expect(t, s.browser().do("GET", G+loc, nil), 404, "")
	if !strings.Contains(owner.do("GET", G+"/mod/queue", nil).Body.String(), "first post") {
		t.Fatal("held post not in the queue")
	}
	expect(t, owner.do("POST", G+"/mod/queue/approve", url.Values{"kind": {"post"}, "id": {idOf(loc)}}), 303, "")
	expect(t, s.browser().do("GET", G+loc, nil), 200, "")
	if !strings.Contains(bob.do("GET", "https://nfb.group/notifications", nil).Body.String(), "Your post “First post” is up") {
		t.Fatal("author not told the held post is up")
	}
}
