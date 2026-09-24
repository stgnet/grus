package web

import (
	"net/http"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/store"
)

// viewer builds the access-rule view of the current user for one group.
func (s *Server) viewer(u *store.User, groupID int64) (auth.Viewer, error) {
	if u == nil {
		return auth.Viewer{}, nil
	}
	v := auth.Viewer{UserID: u.ID, Operator: u.IsOperator}
	m, err := s.Store.Membership(groupID, u.ID)
	if err != nil {
		return v, err
	}
	if m != nil {
		v.Role, v.Status = m.Role, m.Status
	}
	return v, nil
}

type groupCard struct {
	Name        string
	Description string
	URL         string
	Visibility  string
}

// home is the bare primary domain: a short splash, sign-in, and the groups
// this visitor can see. (The root FAQ joins it in M3.)
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	u := s.user(r)
	groups, err := s.Store.Groups()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var cards []groupCard
	for i := range groups {
		g := &groups[i]
		st, err := s.Store.GroupSettings(g.ID)
		if err != nil || st == nil {
			continue // a group whose file isn't here yet; skip it rather than fail the page
		}
		v, err := s.viewer(u, g.ID)
		if err != nil || !auth.CanSeeGroup(v, st.Visibility) {
			continue
		}
		cards = append(cards, groupCard{Name: g.Name, Description: st.Description,
			URL: s.groupURL(g, rt.primary, "/"), Visibility: st.Visibility})
	}
	s.render(w, r, http.StatusOK, "home", &page{Title: rt.primary, User: u, Data: cards})
}

type groupData struct {
	Settings *store.Settings
	CanRead  bool
	Rules    []string
}

// groupHome is a group's front page. In M0 it's the group's name,
// description and rules; posts and the feed arrive in M1.
func (s *Server) groupHome(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	u := s.user(r)
	st, err := s.Store.GroupSettings(rt.group.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v, err := s.viewer(u, rt.group.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if st == nil || !auth.CanSeeGroup(v, st.Visibility) {
		// Hidden groups answer exactly like groups that don't exist.
		s.render(w, r, http.StatusNotFound, "notfound", &page{Title: "No such group", User: u})
		return
	}
	var rules []string
	for _, line := range strings.Split(st.Rules, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rules = append(rules, line)
		}
	}
	s.render(w, r, http.StatusOK, "group", &page{Title: rt.group.Name, User: u, Group: rt.group,
		Data: groupData{Settings: st, CanRead: auth.CanRead(v, st.Visibility, nil), Rules: rules}})
}

// groupLogin sends sign-in to the primary domain, coming back here after.
func (s *Server) groupLogin(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	back := s.groupURL(rt.group, rt.primary, "/")
	http.Redirect(w, r, s.primaryURL(rt.primary, "/login?next="+queryEscape(back)), http.StatusSeeOther)
}
