package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The mods' pages and controls (plan section 6, "Mod tools"): the queue,
// the mod log, roles and bans, lock and pin, remove with a reason; and
// the members' side: reports and Keep / Hide votes on flagged items.

type queueView struct {
	store.QueueItem
	Author string
	URL    string
}

type modQueueData struct {
	Items    []queueView
	Requests int // join requests, on the Members page
}

// modQueue is everything waiting on the mods, oldest first.
func (s *Server) modQueue(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	items, err := s.Store.ModQueue(c.g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var ids []int64
	for _, q := range items {
		ids = append(ids, q.UserID)
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := modQueueData{}
	for _, q := range items {
		v := queueView{QueueItem: q, Author: authorOf(names, q.UserID, false), URL: fmt.Sprintf("/p/%d", q.PostID)}
		if q.Kind == "comment" {
			v.URL += fmt.Sprintf("#c%d", q.ID)
		}
		d.Items = append(d.Items, v)
	}
	reqs, _ := s.Store.JoinRequests(c.g.ID)
	d.Requests = len(reqs)
	s.render(w, r, http.StatusOK, "mod-queue", c.page("Mod queue", d))
}

// modQueueAction: approve (let it stand) or remove, from the queue.
func (s *Server) modQueueAction(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	kind := r.FormValue("kind")
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if kind != "post" && kind != "comment" {
		s.notFound(w, r)
		return
	}
	var err error
	switch r.PathValue("action") {
	case "approve":
		_, err = s.Log.Apply(&cmd.Approve{GroupID: c.g.ID, Kind: kind, ID: id, By: c.u.ID, At: s.Now().Unix()})
	case "remove":
		_, err = s.Log.Apply(&cmd.SoftDelete{GroupID: c.g.ID, Kind: kind, ID: id, By: c.u.ID, ByMod: true,
			Reason: strings.TrimSpace(r.FormValue("reason")), At: s.Now().Unix()})
	default:
		s.notFound(w, r)
		return
	}
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't do that"}) {
		return
	}
	http.Redirect(w, r, "/mod/queue", http.StatusSeeOther)
}

type logView struct {
	store.LogEntry
	Actor string // a handle, or "auto"
	URL   string // the target, where there's a page for it
}

type modLogData struct {
	Entries  []logView
	NextPage int
}

// modLogPage shows the mod log: every mod action, the AI's included
// ("auto"), with its reason. Visible to the group's mods.
func (s *Server) modLogPage(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	const size = 100
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pg = max(pg, 0)
	entries, err := s.Store.ModLog(c.g.ID, size+1, pg*size)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := modLogData{}
	if len(entries) > size {
		entries, d.NextPage = entries[:size], pg+1
	}
	var ids []int64
	for _, e := range entries {
		ids = append(ids, e.ActorID)
		if e.TargetType == "user" {
			ids = append(ids, e.TargetID)
		}
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, e := range entries {
		v := logView{LogEntry: e, Actor: "auto"}
		if e.ActorID != 0 {
			v.Actor = authorOf(names, e.ActorID, false)
		}
		switch e.TargetType {
		case "post":
			v.URL = fmt.Sprintf("/p/%d", e.TargetID)
		case "comment":
			if cm, err := s.Store.Comment(c.g.ID, e.TargetID); err == nil && cm != nil {
				v.URL = fmt.Sprintf("/p/%d#c%d", cm.PostID, cm.ID)
			}
		case "user":
			// Named rather than linked: there are no profile pages to go to.
			v.Reason = strings.TrimSpace(authorOf(names, e.TargetID, false) + " " + v.Reason)
		}
		d.Entries = append(d.Entries, v)
	}
	s.render(w, r, http.StatusOK, "mod-log", c.page("Mod log", d))
}

type memberView struct {
	store.Member
	Handle string
}

// memberAction: an owner changes a member's role; a mod bans or unbans.
func (s *Server) memberAction(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	at := s.Now().Unix()
	var err error
	target, _ := strconv.ParseInt(r.FormValue("user"), 10, 64)
	if name := strings.TrimSpace(r.FormValue("handle")); name != "" {
		u, e := s.Store.UserByName(name)
		if e == nil && u == nil {
			e = cmd.Invalid("there's no one called %s", name)
		}
		if e != nil {
			s.membersFailed(w, r, c, e)
			return
		}
		target = u.ID
	}
	switch r.PathValue("action") {
	case "role":
		if !auth.CanManage(c.v) {
			s.notFound(w, r)
			return
		}
		_, err = s.Log.Apply(&cmd.SetRole{GroupID: c.g.ID, UserID: target, Role: r.FormValue("role"), By: c.u.ID, At: at})
	case "ban":
		var until int64
		if days, _ := strconv.Atoi(r.FormValue("days")); days > 0 {
			until = at + int64(days)*86400
		}
		_, err = s.Log.Apply(&cmd.Ban{GroupID: c.g.ID, UserID: target, Until: until, Reason: r.FormValue("reason"),
			By: c.u.ID, ByOperator: c.v.Operator, At: at})
	case "unban":
		_, err = s.Log.Apply(&cmd.Unban{GroupID: c.g.ID, UserID: target, By: c.u.ID, At: at})
	default:
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.membersFailed(w, r, c, err)
		return
	}
	http.Redirect(w, r, "/mod/members#members", http.StatusSeeOther)
}

func (s *Server) membersFailed(w http.ResponseWriter, r *http.Request, c *greq, err error) {
	if !cmd.IsInput(err) {
		s.serverError(w, r, err)
		return
	}
	d, derr := s.membersData(c)
	if derr != nil {
		s.serverError(w, r, derr)
		return
	}
	s.commandFailed(w, r, c, err, "mod-members", d)
}

// postFlag locks or pins a post (mods).
func (s *Server) postFlag(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	flag := map[string]string{"lock": "locked", "pin": "pinned"}[r.PathValue("flag")]
	if flag == "" {
		s.notFound(w, r)
		return
	}
	_, err := s.Log.Apply(&cmd.SetPostFlag{GroupID: c.g.ID, PostID: p.ID, Flag: flag, On: r.FormValue("on") == "1", By: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't do that"}) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}

type reportData struct {
	Kind   string
	ID     int64
	PostID int64
	Done   bool
}

// itemRef reads a /p/{id}/... or /c/{id}/... path's item, applying the
// read rule.
func (s *Server) itemRef(w http.ResponseWriter, r *http.Request, c *greq) (kind string, id, postID int64, ok bool) {
	if strings.HasPrefix(r.URL.Path, "/c/") {
		cm := s.loadComment(w, r, c)
		if cm == nil {
			return "", 0, 0, false
		}
		return "comment", cm.ID, cm.PostID, true
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return "", 0, 0, false
	}
	return "post", p.ID, p.ID, true
}

// reportForm and report: a member reports a post or comment.
func (s *Server) reportForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	kind, id, postID, ok := s.itemRef(w, r, c)
	if !ok {
		return
	}
	if !s.writer(w, r, c, r.URL.Path) {
		return
	}
	s.render(w, r, http.StatusOK, "report", c.page("Report", reportData{Kind: kind, ID: id, PostID: postID}))
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	kind, id, postID, ok := s.itemRef(w, r, c)
	if !ok || !s.writer(w, r, c, r.URL.Path) {
		return
	}
	d := reportData{Kind: kind, ID: id, PostID: postID}
	_, err := s.Log.Apply(&cmd.Report{GroupID: c.g.ID, Kind: kind, ID: id, UserID: c.u.ID, Reason: r.FormValue("reason"), At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "report", d) {
		return
	}
	d.Done = true
	s.render(w, r, http.StatusOK, "report", c.page("Reported", d))
}

// vote is a member's Keep or Hide on a flagged item.
func (s *Server) vote(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	kind, id, postID, ok := s.itemRef(w, r, c)
	if !ok || !s.writer(w, r, c, fmt.Sprintf("/p/%d", postID)) {
		return
	}
	_, err := s.Log.Apply(&cmd.Vote{GroupID: c.g.ID, Kind: kind, ID: id, UserID: c.u.ID, Vote: r.FormValue("vote"), At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't vote"}) {
		return
	}
	back := fmt.Sprintf("/p/%d", postID)
	if kind == "comment" {
		back += fmt.Sprintf("#c%d", id)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// New-account limits (plan section 6, "Anti-spam"): accounts under three
// days old, or with nothing shown in the group yet, can't post links and
// can post only a few times an hour. The limits lift by themselves.
const (
	newAccountAge   = 3 * 86400
	newAccountPosts = 5 // posts and comments per hour
)

var linkRE = regexp.MustCompile(`(?i)(https?://|www\.)\S+|\b[a-z0-9-]+\.(com|net|org|io|co|info|biz|ru|cn|xyz|top)\b`)

// newAccountLimit returns what a new account can't do with this text, or
// "" when it's fine. It counts the attempt against the hourly limit.
func (s *Server) newAccountLimit(c *greq, text string) string {
	if c.mod() {
		return ""
	}
	now := s.Now().Unix()
	isNew := now-c.u.CreatedAt < newAccountAge
	if !isNew {
		shown, err := s.Store.HasShownContent(c.g.ID, c.u.ID)
		isNew = err == nil && !shown
	}
	if !isNew {
		return ""
	}
	if linkRE.MatchString(text) {
		return "New accounts can't post links yet. Post without the link; it becomes possible after a few days, once you've taken part."
	}
	if !s.newPosts.allow(c.u.ID, now) {
		return "New accounts can post a few times an hour. Try again a little later."
	}
	return ""
}

// hourCounter counts actions per account in the current hour, in memory
// on the node serving them (like the Ask limit: close enough for what it's
// for, and nothing to replicate).
type hourCounter struct {
	mu    sync.Mutex
	hour  int64
	count map[int64]int
}

func (h *hourCounter) allow(userID, now int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hour := now / 3600; hour != h.hour {
		h.hour, h.count = hour, map[int64]int{}
	}
	if h.count[userID] >= newAccountPosts {
		return false
	}
	h.count[userID]++
	return true
}

// adminSuspend suspends an account site-wide, or lifts a suspension (the
// operator only; bans are per group and belong to its mods).
func (s *Server) adminSuspend(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	name := strings.TrimSpace(r.FormValue("who"))
	form := map[string]string{"who": name}
	target, err := s.Store.UserByName(name)
	if err == nil && target == nil {
		err = cmd.Invalid("there's no account %s", name)
	}
	if err == nil {
		// days: a number of days; 0 until lifted (a hundred years is
		// forever enough, and keeps "until" a plain time); -1 lifts it.
		var until int64
		switch days, _ := strconv.Atoi(r.FormValue("days")); {
		case days > 0:
			until = s.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
		case days == 0:
			until = s.Now().AddDate(100, 0, 0).Unix()
		}
		_, err = s.Log.Apply(&cmd.SuspendUser{UserID: target.ID, Until: until, At: s.Now().Unix()})
	}
	if err != nil {
		s.adminFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
