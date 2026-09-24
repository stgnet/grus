package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// greq is what every handler on a group's host starts from: the group, its
// settings, who's asking, and what they are in this group.
type greq struct {
	rt   *route
	g    *store.Group
	st   *store.Settings
	u    *store.User // nil when signed out
	v    auth.Viewer
	root bool // the root FAQ on the bare domain, not a real group
}

// group loads the request's group context. It writes a 404 and returns nil
// when the viewer can't see the group at all, so a hidden group answers
// exactly like one that doesn't exist.
func (s *Server) group(w http.ResponseWriter, r *http.Request) *greq {
	rt := routeOf(r)
	c := &greq{rt: rt, g: rt.group, u: s.user(r)}
	var err error
	if c.st, err = s.Store.GroupSettings(c.g.ID); err != nil {
		s.serverError(w, r, err)
		return nil
	}
	if c.v, err = s.viewer(c.u, c.g.ID); err != nil {
		s.serverError(w, r, err)
		return nil
	}
	if c.st == nil || !auth.CanSeeGroup(c.v, c.st.Visibility) {
		s.render(w, r, http.StatusNotFound, "notfound", &page{Title: "No such group", User: c.u})
		return nil
	}
	return c
}

func (c *greq) canRead(item *auth.Item) bool { return auth.CanRead(c.v, c.st.Visibility, item) }
func (c *greq) mod() bool                    { return auth.CanModerate(c.v) }
func (c *greq) member() bool                 { return auth.IsMember(c.v) }

// page starts a page for this group.
func (c *greq) page(title string, data any) *page {
	if c.root {
		return &page{Title: title, User: c.u, Data: data}
	}
	return &page{Title: title, User: c.u, Group: c.g, Data: data, Manage: auth.CanManage(c.v),
		// Groups aren't listed by search engines unless their owners say so.
		NoIndex: !c.st.AllowIndexing || c.st.Visibility != "public"}
}

// writer checks that the request can write in this group: signed in, with a
// handle, and an active member. In an open group, the first write joins
// them (one less step between reading and replying). It writes the
// response and returns false when they can't.
func (s *Server) writer(w http.ResponseWriter, r *http.Request, c *greq, back string) bool {
	if c.u == nil {
		login := s.primaryURL(c.rt.primary, "/login?next="+queryEscape(s.groupURL(c.g, c.rt.primary, back)))
		http.Redirect(w, r, login, http.StatusSeeOther)
		return false
	}
	if c.u.Handle == "" {
		http.Redirect(w, r, s.primaryURL(c.rt.primary, "/welcome?next="+queryEscape(s.groupURL(c.g, c.rt.primary, back))), http.StatusSeeOther)
		return false
	}
	if c.member() {
		return true
	}
	if c.v.Status == "" && c.st.JoinPolicy == "open" {
		v, err := s.Log.Apply(&cmd.JoinGroup{GroupID: c.g.ID, UserID: c.u.ID, At: s.Now().Unix()})
		if err != nil {
			s.serverError(w, r, err)
			return false
		}
		if v == "active" {
			c.v.Role, c.v.Status = "member", "active"
			return true
		}
	}
	s.render(w, r, http.StatusForbidden, "message", c.page("Members only",
		message{Title: "Members only", Text: "Only members can post here. Ask to join from the group's page."}))
	return false
}

// message is a page that's just a title and a sentence.
type message struct {
	Title string
	Text  string
}

type feedData struct {
	Settings    *store.Settings
	CanRead     bool
	IsMember    bool
	Pending     bool
	Posts       []postCard
	Sort        string
	Window      string // Top: week | month | year | all
	NextPage    int
	MemberCount int
	FAQEntries  int  // the FAQ, pinned at the top of the feed
	Newcomer    bool // not a member: "New here? Start with the FAQ"
	CanMod      bool
	Requests    int // for mods: people asking to join
	Queue       int // for mods: flagged, reported and held items
}

type postCard struct {
	store.Post
	Author  string
	Excerpt string
}

const feedPageSize = 30

// groupHome is a group's front page: its feed.
func (s *Server) groupHome(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	sort := r.URL.Query().Get("sort")
	if sort != store.SortNew && sort != store.SortTop {
		sort = store.SortActive
	}
	window := r.URL.Query().Get("t")
	if _, ok := store.TopWindows[window]; !ok {
		window = "month"
	}
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pg < 0 {
		pg = 0
	}
	d := feedData{Settings: c.st, CanRead: c.canRead(nil), IsMember: c.member(), Pending: c.v.Status == "pending", Sort: sort, Window: window}
	d.MemberCount, _ = s.Store.MemberCount(c.g.ID)
	d.CanMod = c.mod()
	if d.CanMod {
		reqs, _ := s.Store.JoinRequests(c.g.ID)
		d.Requests = len(reqs)
		queue, _ := s.Store.ModQueue(c.g.ID)
		d.Queue = len(queue)
	}
	if auth.CanReadFAQ(c.v, c.st.Visibility, c.st.PublicFAQ) {
		d.FAQEntries, _ = s.Store.EntryCount(c.g.ID)
		d.Newcomer = !d.IsMember && d.FAQEntries > 0
	}
	if d.CanRead {
		var posts []store.Post
		var err error
		if sort == store.SortTop {
			var since int64
			if w := store.TopWindows[window]; w > 0 {
				since = s.Now().Unix() - w
			}
			posts, err = s.Store.FeedTop(c.g.ID, since, feedPageSize+1, pg*feedPageSize)
		} else {
			posts, err = s.Store.Feed(c.g.ID, sort, feedPageSize+1, pg*feedPageSize)
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if len(posts) > feedPageSize {
			posts = posts[:feedPageSize]
			d.NextPage = pg + 1
		}
		names, err := s.authorNames(posts, nil)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, p := range posts {
			d.Posts = append(d.Posts, postCard{Post: p, Author: authorOf(names, p.UserID, p.Anonymous), Excerpt: excerpt(p.Body, 160)})
		}
	}
	s.render(w, r, http.StatusOK, "group", c.page(c.g.Name, d))
}

// authorNames looks up handles for the authors of posts and comments.
func (s *Server) authorNames(posts []store.Post, comments []store.Comment) (map[int64]string, error) {
	var ids []int64
	for _, p := range posts {
		if p.UserID != 0 {
			ids = append(ids, p.UserID)
		}
	}
	for _, c := range comments {
		if c.UserID != 0 {
			ids = append(ids, c.UserID)
		}
	}
	return s.Store.Handles(ids)
}

// authorOf is how an author is shown: their handle, "Anonymous member",
// the archive label for imported posts, or "[deleted]" for an account
// that's gone.
func authorOf(names map[int64]string, id int64, anonymous bool) string {
	switch {
	case anonymous:
		return "Anonymous member"
	case id == 0: // only archive posts have no account
		return cmd.ArchiveAuthorHandle
	case names[id] == "":
		return "[deleted]"
	}
	return names[id]
}

type aboutData struct {
	Settings    *store.Settings
	Rules       []string
	Mods        []string
	MemberCount int
	Sisters     []sisterView
	IsMember    bool
}

// groupAbout shows the description, rules, and who the mods are.
func (s *Server) groupAbout(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	d := aboutData{Settings: c.st}
	for _, line := range strings.Split(c.st.Rules, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			d.Rules = append(d.Rules, line)
		}
	}
	ids, err := s.Store.Mods(c.g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, id := range ids {
		if n := names[id]; n != "" {
			d.Mods = append(d.Mods, n)
		}
	}
	d.MemberCount, _ = s.Store.MemberCount(c.g.ID)
	if d.Sisters, err = s.sisterViews(c, true); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.IsMember = c.member() || c.v.Status == "pending"
	s.render(w, r, http.StatusOK, "about", c.page("About "+c.g.Name, d))
}

// pathID reads a numeric id from the path.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}
