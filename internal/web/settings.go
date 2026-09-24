package web

import (
	"net/http"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Group settings, for the group's owners (plan section 6, "Mod tools").
// Each milestone adds its settings to the form and to cmd's settingRules.

type settingsData struct {
	Settings *store.Settings
}

func (s *Server) owner(w http.ResponseWriter, r *http.Request) *greq {
	c := s.group(w, r)
	if c == nil {
		return nil
	}
	if !auth.CanManage(c.v) {
		s.notFound(w, r)
		return nil
	}
	return c
}

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request) {
	c := s.owner(w, r)
	if c == nil {
		return
	}
	s.render(w, r, http.StatusOK, "settings", c.page("Group settings", settingsData{Settings: c.st}))
}

// settingsSave applies the form. Checkboxes are sent only when ticked, so
// every checkbox on the form is listed here and set either way.
func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	c := s.owner(w, r)
	if c == nil {
		return
	}
	set := map[string]any{}
	for _, k := range []string{"name", "description", "rules"} {
		set[k] = strings.TrimSpace(r.FormValue(k))
	}
	for _, k := range []string{"ai_enabled", "allow_indexing"} {
		set[k] = r.FormValue(k) == "on"
	}
	_, err := s.Log.Apply(&cmd.UpdateSettings{GroupID: c.g.ID, Set: set, By: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "settings", settingsData{Settings: c.st}) {
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// howItWorks is the one place the site says plainly that some of what
// readers see is written automatically (plan section 1, "Disclosure").
func (s *Server) howItWorks(w http.ResponseWriter, r *http.Request) {
	p := &page{Title: "How this site works", User: s.user(r)}
	if rt := routeOf(r); rt != nil && rt.group != nil {
		p.Group = rt.group
	}
	s.render(w, r, http.StatusOK, "how-it-works", p)
}
