package auth

import (
	"testing"
	"time"
)

func TestCanRead(t *testing.T) {
	anon := Viewer{}
	member := Viewer{UserID: 1, Role: "member", Status: "active"}
	pending := Viewer{UserID: 2, Role: "member", Status: "pending"}
	banned := Viewer{UserID: 3, Role: "member", Status: "banned"}
	mod := Viewer{UserID: 4, Role: "mod", Status: "active"}
	author := Viewer{UserID: 5, Role: "member", Status: "active"}
	op := Viewer{UserID: 6, Operator: true}

	item := func(status string) *Item { return &Item{AuthorID: 5, Status: status} }

	cases := []struct {
		name       string
		v          Viewer
		visibility string
		item       *Item
		want       bool
	}{
		{"anyone reads a public group", anon, "public", nil, true},
		{"anyone reads a visible public post", anon, "public", item("visible"), true},
		{"flagged posts stay readable", anon, "public", item("flagged"), true},
		{"signed-out can't read private", anon, "private", nil, false},
		{"pending member can't read private", pending, "private", nil, false},
		{"member reads private", member, "private", item("visible"), true},
		{"member reads hidden", member, "hidden", nil, true},
		{"banned member reads nothing", banned, "public", item("visible"), false},
		{"held: not other members", member, "public", item("held"), false},
		{"held: the author", author, "public", item("held"), true},
		{"held: mods", mod, "public", item("held"), true},
		{"removed: not the author", author, "public", item("removed"), false},
		{"removed: mods", mod, "public", item("removed"), true},
		{"deleted: not readers", anon, "public", item("deleted"), false},
		{"operator reads everything", op, "hidden", item("deleted"), true},
		{"unknown status fails closed", mod, "public", item("bogus"), false},
	}
	for _, c := range cases {
		if got := CanRead(c.v, c.visibility, c.item); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanSeeGroup(t *testing.T) {
	if !CanSeeGroup(Viewer{}, "private") {
		t.Error("private groups' names are public")
	}
	if CanSeeGroup(Viewer{}, "hidden") {
		t.Error("hidden groups are invisible to non-members")
	}
	if !CanSeeGroup(Viewer{UserID: 1, Role: "member", Status: "active"}, "hidden") {
		t.Error("members see their hidden group")
	}
	if CanSeeGroup(Viewer{UserID: 1, Role: "member", Status: "banned"}, "public") {
		t.Error("banned members don't see the group")
	}
}

func TestSendLimiter(t *testing.T) {
	l := NewSendLimiter()
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if !l.Allow("a@x.com", "1.1.1.1", now) {
			t.Fatalf("send %d refused", i+1)
		}
	}
	if l.Allow("a@x.com", "2.2.2.2", now) {
		t.Fatal("sixth send to one address allowed")
	}
	if !l.Allow("a@x.com", "1.1.1.1", now.Add(time.Hour)) {
		t.Fatal("limit didn't reset after an hour")
	}
}

func TestCodes(t *testing.T) {
	_, h := NewToken()
	code := NewCode()
	if len(code) != 6 {
		t.Fatalf("code %q is not 6 digits", code)
	}
	stored := CodeHash(h, code)
	if !CodeMatches(h, code, stored) {
		t.Fatal("right code rejected")
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if CodeMatches(h, wrong, stored) {
		t.Fatal("wrong code accepted")
	}
}

// The sister-group visibility rule: only a public group's posts may be
// cited in another group, whatever that group's own visibility.
func TestCanCite(t *testing.T) {
	for vis, want := range map[string]bool{"public": true, "private": false, "hidden": false, "": false} {
		if CanCite(vis) != want {
			t.Errorf("CanCite(%q) = %v", vis, !want)
		}
	}
}
