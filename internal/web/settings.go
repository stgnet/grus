package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Group settings, for the group's owners (plan section 6, "Mod tools").
// Each milestone adds its settings to the form and to cmd's settingRules.

type settingsData struct {
	Settings *store.Settings
	Sisters  []sisterView // proposed and active sister groups
	Saved    bool
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

func (s *Server) settingsData(c *greq) (*settingsData, error) {
	d := &settingsData{Settings: c.st}
	var err error
	d.Sisters, err = s.sisterViews(c, false)
	return d, err
}

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request) {
	c := s.owner(w, r)
	if c == nil {
		return
	}
	d, err := s.settingsData(c)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Saved = r.URL.Query().Get("saved") == "1"
	s.render(w, r, http.StatusOK, "settings", c.page("Group settings", d))
}

// settingsSave applies the form. Checkboxes are sent only when ticked, so
// every checkbox on the form is listed here and set either way.
func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	c := s.owner(w, r)
	if c == nil {
		return
	}
	set := map[string]any{}
	r.ParseForm()
	for _, k := range []string{"name", "description", "rules", "visibility", "join_policy", "join_questions"} {
		if _, sent := r.PostForm[k]; sent {
			set[k] = strings.TrimSpace(r.PostFormValue(k))
		}
	}
	// Checkboxes: an unticked box isn't sent at all, so absent means off.
	for _, k := range []string{"ai_enabled", "allow_indexing", "allow_anonymous", "public_faq", "hold_first_post"} {
		set[k] = r.FormValue(k) == "on"
	}
	if v, sent := r.PostForm["vote_threshold"]; sent {
		// float64, as a number arrives in JSON; the command checks the range.
		n, _ := strconv.ParseFloat(v[0], 64)
		set["vote_threshold"] = n
	}
	// A public group has no separate FAQ setting (its FAQ is public like
	// everything else), so the box isn't on the form; leave it alone, and
	// UpdateSettings turns it off if the group goes private.
	if r.FormValue("visibility") == "public" || c.st.Visibility == "public" {
		delete(set, "public_faq")
	}
	_, err := s.Log.Apply(&cmd.UpdateSettings{GroupID: c.g.ID, Set: set, By: c.u.ID, At: s.Now().Unix()})
	if err != nil && cmd.IsInput(err) {
		d, derr := s.settingsData(c)
		if derr != nil {
			s.serverError(w, r, derr)
			return
		}
		s.commandFailed(w, r, c, err, "settings", d)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
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
