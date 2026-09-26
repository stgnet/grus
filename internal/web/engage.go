package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Engagement pages (M6): the "helpful" button, following a post, the bell
// and its page, and notification settings.

// helpful marks a post or comment helpful, or takes it back.
func (s *Server) helpful(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	kind, id, postID, ok := s.itemRef(w, r, c)
	back := fmt.Sprintf("/p/%d", postID)
	if !ok || !s.writer(w, r, c, back) {
		return
	}
	if kind == "comment" {
		back += fmt.Sprintf("#c%d", id)
	}
	_, err := s.Log.Apply(&cmd.Helpful{GroupID: c.g.ID, Kind: kind, ID: id, UserID: c.u.ID, On: r.FormValue("on") == "1", At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't do that"}) {
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// follow starts or stops following a post.
func (s *Server) follow(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	back := fmt.Sprintf("/p/%d", p.ID)
	if !s.writer(w, r, c, back) {
		return
	}
	_, err := s.Log.Apply(&cmd.Follow{GroupID: c.g.ID, PostID: p.ID, UserID: c.u.ID, On: r.FormValue("on") == "1", At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't do that"}) {
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// unreadCount is the number on the bell: unread notifications across the
// groups on this node. It's one indexed count per group, cheap enough to do
// on every page for the handful of groups a site has; if that ever stops
// being true, this is the place for a short-lived per-user cache.
func (s *Server) unreadCount(u *store.User) int {
	groups, err := s.Store.Groups()
	if err != nil {
		return 0
	}
	n := 0
	for _, g := range groups {
		k, err := s.Store.UnreadCount(g.ID, u.ID)
		if err == nil {
			n += k
		}
	}
	return n
}

// noticeView is a notification as the bell page and emails show it.
type noticeView struct {
	store.Notification
	Group  string
	Text   string
	URL    string
	Unread bool
}

// noticeText words a notification. The words are the same in the bell and
// in email. Titles are quoted as they are now, not as they were.
func noticeText(n store.Notification, groupName string, names map[int64]string) string {
	actor := authorOf(names, n.ActorID, n.Anonymous)
	title := "“" + n.PostTitle + "”"
	what := n.RefKind
	switch n.Kind {
	case cmd.NoteReply:
		return actor + " replied to your comment on " + title
	case cmd.NoteComment:
		if n.PostAuthor == n.UserID {
			return actor + " commented on your post " + title
		}
		return actor + " commented on " + title
	case cmd.NoteLinked:
		return "Newer information was linked to " + title
	case cmd.NoteJoined:
		return "You're in: your request to join " + groupName + " was approved"
	case cmd.NoteHidden:
		return "Your " + what + " in " + title + " is hidden from others while moderators review it"
	case cmd.NoteRemoved:
		return "A moderator removed your " + what + " in " + title
	case cmd.NoteApproved:
		return "Your post " + title + " is up: a moderator let it through"
	}
	return "Something happened in " + groupName
}

// noticePath is where a notification leads, on its group's host.
func noticePath(n store.Notification) string {
	switch {
	case n.Kind == cmd.NoteJoined:
		return "/"
	case n.Kind == cmd.NoteLinked:
		return fmt.Sprintf("/p/%d", n.RefID)
	case n.RefKind == "comment":
		return fmt.Sprintf("/p/%d#c%d", n.PostID, n.RefID)
	}
	return fmt.Sprintf("/p/%d", n.PostID)
}

// noticeViews words a list of notifications, all from one group.
func (s *Server) noticeViews(g *store.Group, domain string, list []store.Notification) ([]noticeView, error) {
	var ids []int64
	for _, n := range list {
		ids = append(ids, n.ActorID)
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		return nil, err
	}
	var out []noticeView
	for _, n := range list {
		out = append(out, noticeView{Notification: n, Group: g.Name, Text: noticeText(n, g.Name, names),
			URL: s.groupURL(g, domain, noticePath(n)), Unread: n.ReadAt == 0})
	}
	return out, nil
}

// notificationsPage is the bell's page, on the bare domain: the newest
// notifications from every group, newest first. Opening it marks what it
// shows as read (up to the newest shown in each group, so anything that
// arrives meanwhile stays unread); they're still highlighted this once.
func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+queryEscape(s.currentURL(r)), http.StatusSeeOther)
		return
	}
	groups, err := s.Store.Groups()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var all []noticeView
	for i := range groups {
		g := &groups[i]
		list, err := s.Store.Notifications(g.ID, u.ID, 50)
		if err != nil || len(list) == 0 {
			continue // a group whose file isn't on this node, or nothing there
		}
		views, err := s.noticeViews(g, rt.domain, list)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		all = append(all, views...)
		if list[0].ReadAt == 0 {
			if _, err := s.Log.Apply(&cmd.MarkRead{GroupID: g.ID, UserID: u.ID, UpTo: list[0].ID, At: s.Now().Unix()}); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	if len(all) > 100 {
		all = all[:100]
	}
	s.render(w, r, http.StatusOK, "notifications", &page{Title: "Notifications", User: u, Data: all})
}

type profileData struct {
	Saved bool
}

// profile is where someone sets how they hear about things. (Photos and
// bios come later; the handle is set on the welcome page.)
func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+queryEscape(s.currentURL(r)), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "profile", &page{Title: "Your settings", User: u,
		Data: profileData{Saved: r.URL.Query().Get("saved") == "1"}})
}

func (s *Server) profileSave(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_, err := s.Log.Apply(&cmd.SetNotifyPrefs{UserID: u.ID, Email: r.FormValue("email") == "on",
		Digest: strings.TrimSpace(r.FormValue("digest")), At: s.Now().Unix()})
	if cmd.IsInput(err) {
		s.render(w, r, http.StatusBadRequest, "profile", &page{Title: "Your settings", User: u,
			Error: capitalize(cmdMessage(err)) + "."})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/profile?saved=1", http.StatusSeeOther)
}
