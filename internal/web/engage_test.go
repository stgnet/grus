package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cmd"
)

func TestNotificationsFollowAndHelpful(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	alice := s.signedIn("alice@example.com", "alice")
	bob := s.signedIn("bob@example.com", "bob")
	carol := s.signedIn("carol@example.com", "carol")
	s.join("carol")

	loc := alice.upload(G+"/submit", map[string]string{"title": "Fridge fan rattle"}, nil).Header().Get("Location")
	postURL := G + loc

	// bob comments: alice (who follows her own post) hears about it.
	bob.upload(postURL+"/comment", map[string]string{"body": "Try a Noctua."}, nil)
	if !strings.Contains(alice.do("GET", G+"/", nil).Body.String(), "Notifications (1)") {
		t.Fatal("no unread count on the bell")
	}
	page := alice.do("GET", "https://nfb.group/notifications", nil).Body.String()
	if !strings.Contains(page, "bob commented on your post “Fridge fan rattle”") || !strings.Contains(page, `class="unread"`) {
		t.Fatalf("notifications page:\n%s", page)
	}
	// Opening the page marked it read.
	if strings.Contains(alice.do("GET", G+"/", nil).Body.String(), "Notifications (1)") {
		t.Fatal("still unread after opening the page")
	}

	// carol follows; alice replies to bob: bob gets "replied", carol
	// "commented", and alice nothing (it's her own).
	expect(t, carol.do("POST", postURL+"/follow", url.Values{"on": {"1"}}), 303, loc)
	if !strings.Contains(carol.do("GET", postURL, nil).Body.String(), "Unfollow") {
		t.Fatal("no Unfollow after following")
	}
	db, _ := s.st.Group(42)
	var bobComment int64
	db.QueryRow(`SELECT id FROM comments WHERE body = 'Try a Noctua.'`).Scan(&bobComment)
	alice.upload(postURL+"/comment", map[string]string{"body": "Which size?", "parent": fmt.Sprint(bobComment)}, nil)
	if p := bob.do("GET", "https://nfb.group/notifications", nil).Body.String(); !strings.Contains(p, "alice replied to your comment") {
		t.Fatalf("bob's notifications:\n%s", p)
	}
	if p := carol.do("GET", "https://nfb.group/notifications", nil).Body.String(); !strings.Contains(p, "alice commented on “Fridge fan rattle”") {
		t.Fatalf("carol's notifications:\n%s", p)
	}
	var own int
	db.QueryRow(`SELECT COUNT(*) FROM notifications n JOIN posts p ON p.user_id = n.user_id WHERE n.actor_id = n.user_id`).Scan(&own)
	if own != 0 {
		t.Fatal("someone was notified of their own action")
	}

	// Helpful: carol marks the post; it counts, and leads the Top sort.
	expect(t, carol.do("POST", postURL+"/helpful", url.Values{"on": {"1"}}), 303, loc)
	if !strings.Contains(carol.do("GET", postURL, nil).Body.String(), `aria-pressed="true">Helpful · 1`) {
		t.Fatal("helpful vote not shown")
	}
	carol.upload(G+"/submit", map[string]string{"title": "Tire pressure"}, nil)
	top := s.browser().do("GET", G+"/?sort=top&t=week", nil).Body.String()
	if strings.Index(top, "Fridge fan rattle") > strings.Index(top, "Tire pressure") {
		t.Fatal("Top sort isn't by helpful votes")
	}
	// Authors can't mark their own.
	expect(t, alice.do("POST", postURL+"/helpful", url.Values{"on": {"1"}}), 400, "")
	// Taking it back.
	expect(t, carol.do("POST", postURL+"/helpful", url.Values{"on": {"0"}}), 303, loc)
	var score int
	db.QueryRow(`SELECT score FROM posts WHERE id = ?`, idOf(loc)).Scan(&score)
	if score != 0 {
		t.Fatalf("score %d after taking the vote back", score)
	}

	// A newer post linked to one carol follows tells her.
	newer := bob.upload(G+"/submit", map[string]string{"title": "Fan fixed with a Noctua"}, nil).Header().Get("Location")
	var older, newerID int64
	db.QueryRow(`SELECT id FROM posts WHERE title = 'Fridge fan rattle'`).Scan(&older)
	db.QueryRow(`SELECT id FROM posts WHERE title = 'Fan fixed with a Noctua'`).Scan(&newerID)
	must(t, s.log, &cmd.AddLink{GroupID: 42, PostA: newerID, PostB: older, Source: "mod", NoteA: 91, NoteB: 92, CombinedA: 93, CombinedB: 94, At: 5})
	p := carol.do("GET", "https://nfb.group/notifications", nil).Body.String()
	if !strings.Contains(p, "Newer information was linked to “Fridge fan rattle”") || !strings.Contains(p, newer) {
		t.Fatalf("no link notification:\n%s", p)
	}
}

func TestNotificationEmailAndDigest(t *testing.T) {
	s := newSite(t)
	G := "https://travato.nfb.group"
	alice := s.signedIn("alice@example.com", "alice")
	bob := s.signedIn("bob@example.com", "bob")
	loc := alice.upload(G+"/submit", map[string]string{"title": "Solar wiring"}, nil).Header().Get("Location")

	// Off by default: nothing is emailed.
	bob.upload(G+loc+"/comment", map[string]string{"body": "Use 10 AWG."}, nil)
	s.srv.Now = func() time.Time { return time.Now().Add(time.Hour) }
	s.mail.Reset()
	if err := s.srv.SendNotices(); err != nil {
		t.Fatal(err)
	}
	if s.mail.Len() != 0 {
		t.Fatalf("emailed without asking:\n%s", s.mail.String())
	}

	// alice turns email on: the waiting notification goes out, once.
	s.srv.Now = time.Now
	expect(t, alice.do("POST", "https://nfb.group/profile", url.Values{"email": {"on"}, "digest": {"daily"}}), 303, "/profile?saved=1")
	s.srv.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := s.srv.SendNotices(); err != nil {
		t.Fatal(err)
	}
	sent := s.mail.String()
	if !strings.Contains(sent, "To: alice@example.com") || !strings.Contains(sent, "bob commented on your post") ||
		!strings.Contains(sent, "https://travato.nfb.group"+loc) || !strings.Contains(sent, "/profile") {
		t.Fatalf("notification email:\n%s", sent)
	}
	s.mail.Reset()
	s.srv.SendNotices()
	if s.mail.Len() != 0 {
		t.Fatal("same notification emailed twice")
	}

	// The digest: the day's posts, then not again the same day.
	if err := s.srv.SendDigests(); err != nil {
		t.Fatal(err)
	}
	if d := s.mail.String(); !strings.Contains(d, "Subject: Today in your groups") || !strings.Contains(d, "Solar wiring") {
		t.Fatalf("digest:\n%s", d)
	}
	s.mail.Reset()
	s.srv.SendDigests()
	if s.mail.Len() != 0 {
		t.Fatal("two digests in a day")
	}
}
