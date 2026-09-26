package cmd

import (
	"testing"
)

// TestSettingsLiveInSite: a group's settings are changed in site.db, and
// the group's own file gets a copy, which is what its own commands read.
func TestSettingsLiveInSite(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	if _, err := d.Apply(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: 1}); err != nil {
		t.Fatal(err)
	}
	if LogOf(&UpdateSettings{GroupID: 1}) != SiteLog {
		t.Fatal("UpdateSettings should be on the site log")
	}
	if _, err := d.Apply(&UpdateSettings{GroupID: 1, Set: map[string]any{"name": "Travato Owners", "hold_first_post": true}, By: 9, At: 2}); err != nil {
		t.Fatal(err)
	}
	s, _ := st.GroupSettings(1)
	f, _ := st.GroupFileSettings(1)
	if s == nil || f == nil || s.Name != "Travato Owners" || !s.HoldFirstPost || *s != *f {
		t.Fatalf("site %+v, file copy %+v", s, f)
	}
	if g, _ := st.GroupByID(1); g.Name != "Travato Owners" {
		t.Fatalf("group list's name: %q", g.Name)
	}
	var logged int
	db, _ := st.Group(1)
	db.QueryRow(`SELECT COUNT(*) FROM mod_log WHERE action = 'settings'`).Scan(&logged)
	if logged != 1 {
		t.Fatal("settings change not in the group's mod log")
	}
}

// TestExportSettings: a group made before settings moved to site.db is
// read from its own file until ExportSettings copies it up, and the copy
// never overwrites what site.db already has.
func TestExportSettings(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	if _, err := d.Apply(&CreateGroup{GroupID: 1, Slug: "travato", Name: "Travato", At: 1}); err != nil {
		t.Fatal(err)
	}
	// Make it look like an old group: no row in site.db, and a file of
	// its own with a setting changed back when settings lived there.
	st.Site().Exec(`DELETE FROM group_settings`)
	db, _ := st.Group(1)
	db.Exec(`UPDATE settings SET rules = 'Be kind'`)
	if s, _ := st.GroupSettings(1); s == nil || s.Rules != "Be kind" {
		t.Fatalf("fallback to the group's file: %+v", s)
	}
	if missing, _ := st.GroupsMissingSettings(); len(missing) != 1 {
		t.Fatalf("missing: %v", missing)
	}
	if _, err := d.Apply(&UpdateSettings{GroupID: 1, Set: map[string]any{"rules": "x"}, At: 2}); !IsInput(err) {
		t.Fatalf("change before the copy: %v", err)
	}
	if _, err := d.Apply(&ExportSettings{GroupID: 1, At: 3}); err != nil {
		t.Fatal(err)
	}
	if missing, _ := st.GroupsMissingSettings(); len(missing) != 0 {
		t.Fatalf("still missing: %v", missing)
	}
	st.Site().Exec(`UPDATE group_settings SET rules = 'Newer'`)
	if _, err := d.Apply(&ExportSettings{GroupID: 1, At: 4}); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.GroupSettings(1); s.Rules != "Newer" {
		t.Fatalf("a second export overwrote site.db: %q", s.Rules)
	}
}

// TestSeedGlobalOnce: the seed from grus.conf applies the first time only;
// after that the admin page's values stand.
func TestSeedGlobalOnce(t *testing.T) {
	st := newStore(t)
	d := &Direct{Store: st}
	seed := &SeedGlobal{Domains: []string{"nfb.group"}, MailFrom: "hi@nfb.group",
		Values: map[string]string{"faq_hour": "3", "operators": "a@x.com"}, At: 1}
	if _, err := d.Apply(seed); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Apply(&SetGlobal{Values: map[string]string{"faq_hour": "5"}}); err != nil {
		t.Fatal(err)
	}
	seed.At = 2
	if _, err := d.Apply(seed); err != nil {
		t.Fatal(err)
	}
	g, _ := st.Global()
	if g.FAQHour != 5 || !g.IsOperator("A@x.com") || g.DigestHour != 12 {
		t.Fatalf("global: %+v", g)
	}
	dom, _ := st.DomainNamed("nfb.group")
	if dom == nil || dom.MailFrom != "hi@nfb.group" {
		t.Fatalf("domain: %+v", dom)
	}
	// Bad values and unknown keys are refused; "" goes back to the default.
	for _, v := range []map[string]string{{"faq_hour": "24"}, {"seeded_at": "0"}, {"acme_email": "nope"}} {
		if _, err := d.Apply(&SetGlobal{Values: v}); !IsInput(err) {
			t.Errorf("%v accepted: %v", v, err)
		}
	}
	d.Apply(&SetGlobal{Values: map[string]string{"faq_hour": ""}})
	if g, _ := st.Global(); g.FAQHour != 8 {
		t.Fatalf("faq_hour back to default: %d", g.FAQHour)
	}
}
