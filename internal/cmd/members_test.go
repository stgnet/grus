package cmd

import (
	"testing"
)

func TestJoinPoliciesAndInvites(t *testing.T) {
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
	must(&CreateGroup{GroupID: 1, Slug: "mods", Name: "Travato Mods", Visibility: "private", OwnerID: 1, At: 10})
	s, _ := st.GroupSettings(1)
	if s.Visibility != "private" || s.PublicFAQ {
		t.Fatalf("private group settings: %+v", s)
	}
	g, _ := st.GroupByID(1)
	if g.Visibility != "private" {
		t.Fatal("site.db copy of visibility")
	}

	// Approval with questions: answers required, then pending until a mod
	// lets them in.
	must(&UpdateSettings{GroupID: 1, Set: map[string]any{"join_policy": "approval", "join_questions": "Which year?"}, By: 1, At: 11})
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 2, At: 12}); !IsInput(err) {
		t.Fatalf("join without answers: %v", err)
	}
	if v := must(&JoinGroup{GroupID: 1, UserID: 2, Answers: "2021", At: 12}); v != "pending" {
		t.Fatalf("join: %v", v)
	}
	must(&ReviewJoin{GroupID: 1, UserID: 2, Approve: true, By: 1, At: 13})
	if m, _ := st.Membership(1, 2); m.Status != "active" {
		t.Fatal("approved member not active")
	}
	if _, err := run(&ReviewJoin{GroupID: 1, UserID: 2, Approve: true, By: 1, At: 13}); !IsInput(err) {
		t.Fatal("answering a request twice")
	}

	// Invite only: no way in without a code; a code works until it's used
	// up, and a pending request with a code is let in.
	must(&UpdateSettings{GroupID: 1, Set: map[string]any{"join_policy": "invite"}, By: 1, At: 14})
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 3, At: 15}); !IsInput(err) {
		t.Fatal("joined an invite-only group without an invite")
	}
	must(&CreateInvite{GroupID: 1, Code: "abcdEFGH2345", MaxUses: 1, ExpiresAt: 100, By: 1, At: 15})
	if v := must(&JoinGroup{GroupID: 1, UserID: 3, Invite: "abcdEFGH2345", At: 16}); v != "active" {
		t.Fatalf("invite join: %v", v)
	}
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 4, Invite: "abcdEFGH2345", At: 16}); !IsInput(err) {
		t.Fatal("a one-use invite worked twice")
	}
	must(&CreateInvite{GroupID: 1, Code: "zzzzZZZZ9999", MaxUses: 5, ExpiresAt: 100, By: 1, At: 15})
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 4, Invite: "zzzzZZZZ9999", At: 100}); !IsInput(err) {
		t.Fatal("an expired invite worked")
	}
	must(&RevokeInvite{GroupID: 1, Code: "zzzzZZZZ9999", By: 1, At: 17})
	if _, err := run(&JoinGroup{GroupID: 1, UserID: 4, Invite: "zzzzZZZZ9999", At: 18}); !IsInput(err) {
		t.Fatal("a revoked invite worked")
	}
	if _, err := run(&CreateInvite{GroupID: 1, Code: "short", MaxUses: 1, ExpiresAt: 100, By: 1, At: 15}); !IsInput(err) {
		t.Fatal("bad invite code accepted")
	}

	// The only owner can't leave; a member can.
	if _, err := run(&LeaveGroup{GroupID: 1, UserID: 1, At: 20}); !IsInput(err) {
		t.Fatal("the only owner left")
	}
	must(&LeaveGroup{GroupID: 1, UserID: 3, At: 20})
	if m, _ := st.Membership(1, 3); m != nil {
		t.Fatal("left but still a member")
	}

	// Going public mirrors to site.db; going private again turns the FAQ
	// preview off unless the same change says otherwise.
	must(&UpdateSettings{GroupID: 1, Set: map[string]any{"visibility": "public", "name": "Mods"}, By: 1, At: 21})
	must(&UpdateSettings{GroupID: 1, Set: map[string]any{"public_faq": true}, By: 1, At: 21})
	must(&UpdateSettings{GroupID: 1, Set: map[string]any{"visibility": "hidden"}, By: 1, At: 22})
	s, _ = st.GroupSettings(1)
	g, _ = st.GroupByID(1)
	if s.PublicFAQ || g.Visibility != "hidden" || g.Name != "Mods" {
		t.Fatalf("after going hidden: %+v %+v", s, g)
	}
}

func TestAnonymousAndReveal(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	run := d.Apply
	run(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", OwnerID: 1, At: 10})
	run(&JoinGroup{GroupID: 1, UserID: 2, At: 10})
	if _, err := run(&CreatePost{GroupID: 1, PostID: 100, UserID: 2, Title: "Embarrassing", Anonymous: true, At: 11}); !IsInput(err) {
		t.Fatal("anonymous post in a group that doesn't allow them")
	}
	run(&UpdateSettings{GroupID: 1, Set: map[string]any{"allow_anonymous": true}, By: 1, At: 11})
	if _, err := run(&CreatePost{GroupID: 1, PostID: 100, UserID: 2, Title: "Embarrassing", Anonymous: true, At: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(&RevealAuthor{GroupID: 1, Kind: "post", ID: 100, By: 1, At: 13}); !IsInput(err) {
		t.Fatal("reveal without a reason")
	}
	v, err := run(&RevealAuthor{GroupID: 1, Kind: "post", ID: 100, By: 1, Reason: "spam report", At: 13})
	if err != nil || v != int64(2) {
		t.Fatalf("reveal: %v, %v", v, err)
	}
	db, _ := st.Group(1)
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM mod_log WHERE action = 'reveal_author' AND target_id = 100 AND reason = 'spam report'`).Scan(&n)
	if n != 1 {
		t.Fatal("reveal not in the mod log")
	}
}

func TestSisterPairing(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	run := d.Apply
	run(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: 10})
	run(&CreateGroup{GroupID: 2, Slug: "promaster", Name: "ProMaster", At: 10})
	if _, err := run(&ProposeSister{GroupID: 1, Other: 1, At: 11}); !IsInput(err) {
		t.Fatal("a group paired with itself")
	}
	// "Use AI" must be on in both.
	run(&UpdateSettings{GroupID: 2, Set: map[string]any{"ai_enabled": false}, By: 1, At: 11})
	if _, err := run(&ProposeSister{GroupID: 1, Other: 2, At: 12}); !IsInput(err) {
		t.Fatal("paired with a group that has AI off")
	}
	run(&UpdateSettings{GroupID: 2, Set: map[string]any{"ai_enabled": true}, By: 1, At: 12})
	if v, err := run(&ProposeSister{GroupID: 1, Other: 2, Topics: "chassis,, engine ", By: 1, At: 13}); err != nil || v != "proposed" {
		t.Fatalf("propose: %v %v", v, err)
	}
	// The proposer can't accept its own proposal.
	if _, err := run(&AnswerSister{GroupID: 1, Other: 2, Accept: true, By: 1, At: 14}); !IsInput(err) {
		t.Fatal("proposer accepted its own proposal")
	}
	// The other side proposing back is a yes.
	if v, _ := run(&ProposeSister{GroupID: 2, Other: 1, By: 3, At: 14}); v != "active" {
		t.Fatalf("mutual proposal: %v", v)
	}
	sis, _ := st.Sisters(1)
	if len(sis) != 1 || sis[0].Other != 2 || sis[0].Topics != "chassis, engine" {
		t.Fatalf("sisters: %+v", sis)
	}
	run(&EndSister{GroupID: 2, Other: 1, By: 3, At: 15})
	if sis, _ := st.Sisters(1); len(sis) != 0 {
		t.Fatal("still sisters after ending")
	}
}
