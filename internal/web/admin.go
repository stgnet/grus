package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The operator's page on the primary domain: groups, domains, cluster
// status. Anyone else gets a plain 404, so the page's existence isn't
// advertised.

type adminData struct {
	Groups  []adminGroup
	Domains []store.Domain
	Cluster map[string]string
	Form    map[string]string // values to refill after an error
}

type adminGroup struct {
	store.Group
	URL string
}

// statser is implemented by the Raft node (not by the local test log).
type statser interface{ Stats() map[string]string }

func (s *Server) operator(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.user(r)
	if u == nil || !u.IsOperator {
		s.notFound(w, r)
		return nil
	}
	return u
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	s.renderAdmin(w, r, u, http.StatusOK, "", nil)
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, u *store.User, status int, msg string, form map[string]string) {
	rt := routeOf(r)
	groups, err := s.Store.Groups()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	domains, err := s.Store.Domains()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := adminData{Domains: domains, Form: form}
	for _, g := range groups {
		d.Groups = append(d.Groups, adminGroup{Group: g, URL: s.groupURL(&g, rt.primary, "/")})
	}
	if st, ok := s.Log.(statser); ok {
		all := st.Stats()
		// A few lines worth glancing at, not Raft's whole dump.
		d.Cluster = map[string]string{}
		for _, k := range []string{"state", "term", "commit_index", "applied_index", "last_snapshot_index", "latest_configuration"} {
			d.Cluster[k] = all[k]
		}
	}
	s.render(w, r, status, "admin", &page{Title: "Admin", User: u, Error: msg, Data: d})
}

// adminFail shows a command's error on the admin page. The operator sees
// the error as it is, which is what you want when something's wrong.
func (s *Server) adminFail(w http.ResponseWriter, r *http.Request, u *store.User, err error, form map[string]string) {
	s.renderAdmin(w, r, u, http.StatusBadRequest, capitalize(cmdMessage(err))+".", form)
}

// cmdMessage strips the command name Run puts in front of an error.
func cmdMessage(err error) string {
	msg := err.Error()
	if _, rest, ok := strings.Cut(msg, ": "); ok {
		return rest
	}
	return msg
}

// adminCreateGroup creates a group; the operator becomes its first owner.
// (Self-serve group creation is on the "later" list.)
func (s *Server) adminCreateGroup(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	form := map[string]string{
		"slug":        strings.ToLower(strings.TrimSpace(r.FormValue("slug"))),
		"name":        strings.TrimSpace(r.FormValue("name")),
		"description": strings.TrimSpace(r.FormValue("description")),
	}
	_, err := s.Log.Apply(&cmd.CreateGroup{
		GroupID:     s.IDs.Next(),
		Slug:        form["slug"],
		Name:        form["name"],
		Description: form["description"],
		OwnerID:     u.ID,
		At:          s.Now().Unix(),
	})
	if err != nil {
		s.adminFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminDomain adds an alternate domain or makes a domain the primary.
func (s *Server) adminDomain(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	var c cmd.Command
	switch r.FormValue("action") {
	case "alternate":
		c = &cmd.AddAlternateDomain{Domain: domain, At: s.Now().Unix()}
	case "primary":
		c = &cmd.SetPrimaryDomain{Domain: domain, At: s.Now().Unix()}
	default:
		s.adminFail(w, r, u, errors.New("unknown action"), nil)
		return
	}
	if _, err := s.Log.Apply(c); err != nil {
		s.adminFail(w, r, u, err, map[string]string{"domain": domain})
		return
	}
	// After a primary change this request's host is now an alternate, so
	// this redirect itself goes through the new redirect rules.
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminAlias adds a single host that redirects to a group (or home).
func (s *Server) adminAlias(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	slug := strings.ToLower(strings.TrimSpace(r.FormValue("group")))
	form := map[string]string{"host": host, "group": slug}
	var gid int64
	if slug != "" {
		g, err := s.Store.GroupBySlug(slug)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if g == nil {
			s.adminFail(w, r, u, errors.New("no group "+slug), form)
			return
		}
		gid = g.ID
	}
	if _, err := s.Log.Apply(&cmd.AddHostAlias{Host: host, GroupID: gid, At: s.Now().Unix()}); err != nil {
		s.adminFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
