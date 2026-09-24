package web

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Joining, invites, the mods' members page, and revealing an anonymous
// author (plan section 2, "Reading and joining"; section 6).

type joinData struct {
	Settings *store.Settings
	Invite   string // an invite code being used
	Answers  string
	Done     string // after joining: "active" or "pending"
}

// joinForm is the join page for a group with questions to answer. (An
// open group's Join is one tap on its front page.)
func (s *Server) joinForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	if c.u == nil || c.u.Handle == "" {
		s.writer(w, r, c, "/join") // sign in first, then back here
		return
	}
	if c.member() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "join", c.page("Join "+c.g.Name, joinData{Settings: c.st, Done: c.v.Status}))
}

// groupJoin is the Join button, and the join form's answers.
func (s *Server) groupJoin(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	if c.u == nil || c.u.Handle == "" {
		s.writer(w, r, c, "/join") // sends them to sign in, then back here
		return
	}
	d := joinData{Settings: c.st, Answers: strings.TrimSpace(r.FormValue("answers"))}
	v, err := s.Log.Apply(&cmd.JoinGroup{GroupID: c.g.ID, UserID: c.u.ID, Answers: d.Answers, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "join", d) {
		return
	}
	if v == "pending" {
		d.Done = "pending"
		s.render(w, r, http.StatusOK, "join", c.page("Join "+c.g.Name, d))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// groupLeave ends the reader's membership, or withdraws their request.
func (s *Server) groupLeave(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	if c.u == nil {
		s.notFound(w, r)
		return
	}
	_, err := s.Log.Apply(&cmd.LeaveGroup{GroupID: c.g.ID, UserID: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't leave"}) {
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// inviteCtx loads the group for an invite link. It's the one group page
// that works for a hidden group's non-members, and only with a code that
// still works; anything else is a plain 404, exactly like a group that
// doesn't exist.
func (s *Server) inviteCtx(w http.ResponseWriter, r *http.Request) (*greq, *store.Invite) {
	rt := routeOf(r)
	c := &greq{rt: rt, g: rt.group, u: s.user(r)}
	var err error
	if c.st, err = s.Store.GroupSettings(c.g.ID); err == nil {
		c.v, err = s.viewer(c.u, c.g.ID)
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil
	}
	inv, err := s.Store.Invite(c.g.ID, r.PathValue("code"))
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil
	}
	if c.st == nil || inv == nil || !inv.Usable(s.Now().Unix()) || c.v.Status == "banned" {
		s.render(w, r, http.StatusNotFound, "notfound", &page{Title: "Not found", User: c.u})
		return nil, nil
	}
	return c, inv
}

// invitePage shows the group an invite is for, with a Join button.
func (s *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	c, inv := s.inviteCtx(w, r)
	if c == nil {
		return
	}
	if c.member() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := c.page("Join "+c.g.Name, joinData{Settings: c.st, Invite: inv.Code})
	p.LoginURL = s.primaryURL(c.rt.primary, "/login?next="+queryEscape(s.groupURL(c.g, c.rt.primary, "/invite/"+inv.Code)))
	s.render(w, r, http.StatusOK, "join", p)
}

// inviteJoin joins through an invite: active at once, whatever the policy.
func (s *Server) inviteJoin(w http.ResponseWriter, r *http.Request) {
	c, inv := s.inviteCtx(w, r)
	if c == nil {
		return
	}
	if c.u == nil || c.u.Handle == "" {
		s.writer(w, r, c, "/invite/"+inv.Code)
		return
	}
	_, err := s.Log.Apply(&cmd.JoinGroup{GroupID: c.g.ID, UserID: c.u.ID, Invite: inv.Code, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "join", joinData{Settings: c.st, Invite: inv.Code}) {
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type requestView struct {
	store.JoinRequest
	Handle string
}

type inviteView struct {
	store.Invite
	URL string
}

type modMembersData struct {
	Settings *store.Settings
	Requests []requestView
	Invites  []inviteView
	NewURL   string // the invite just made, to copy
}

// modMembers is the mods' page for join requests and invite links.
func (s *Server) modMembers(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	d, err := s.membersData(c)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if code := r.URL.Query().Get("new"); code != "" {
		d.NewURL = s.groupURL(c.g, c.rt.primary, "/invite/"+code)
	}
	s.render(w, r, http.StatusOK, "mod-members", c.page("Members", d))
}

func (s *Server) membersData(c *greq) (*modMembersData, error) {
	d := &modMembersData{Settings: c.st}
	reqs, err := s.Store.JoinRequests(c.g.ID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range reqs {
		ids = append(ids, r.UserID)
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		d.Requests = append(d.Requests, requestView{JoinRequest: r, Handle: names[r.UserID]})
	}
	invites, err := s.Store.Invites(c.g.ID, s.Now().Unix())
	if err != nil {
		return nil, err
	}
	for _, i := range invites {
		d.Invites = append(d.Invites, inviteView{Invite: i, URL: s.groupURL(c.g, c.rt.primary, "/invite/"+i.Code)})
	}
	return d, nil
}

// modMembersAction: approve or decline a request, make or revoke an invite.
func (s *Server) modMembersAction(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	at := s.Now().Unix()
	user, _ := strconv.ParseInt(r.FormValue("user"), 10, 64)
	var err error
	next := "/mod/members"
	switch r.PathValue("action") {
	case "approve", "decline":
		_, err = s.Log.Apply(&cmd.ReviewJoin{GroupID: c.g.ID, UserID: user, Approve: r.PathValue("action") == "approve", By: c.u.ID, At: at})
	case "invite":
		uses, _ := strconv.Atoi(r.FormValue("uses"))
		days, _ := strconv.Atoi(r.FormValue("days"))
		code := inviteCode()
		_, err = s.Log.Apply(&cmd.CreateInvite{GroupID: c.g.ID, Code: code, MaxUses: uses,
			ExpiresAt: at + int64(days)*86400, By: c.u.ID, At: at})
		next += "?new=" + code
	case "revoke":
		_, err = s.Log.Apply(&cmd.RevokeInvite{GroupID: c.g.ID, Code: r.FormValue("code"), By: c.u.ID, At: at})
	default:
		s.notFound(w, r)
		return
	}
	if err != nil && cmd.IsInput(err) {
		d, derr := s.membersData(c)
		if derr != nil {
			s.serverError(w, r, derr)
			return
		}
		s.commandFailed(w, r, c, err, "mod-members", d)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// inviteCode is a random invite code: 16 letters and digits, about 95 bits,
// so codes can't be guessed. Made here, not in the command, because Apply
// must be deterministic.
func inviteCode() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err) // crypto/rand doesn't fail on supported systems
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

type revealData struct {
	Kind   string
	ID     int64
	PostID int64
	Handle string // after the reveal
}

// revealForm and reveal: a mod finds out who wrote an anonymous post or
// comment. They must give a reason, and it goes in the mod log.
func (s *Server) revealForm(w http.ResponseWriter, r *http.Request) {
	c, d := s.revealCtx(w, r)
	if c == nil {
		return
	}
	s.render(w, r, http.StatusOK, "reveal", c.page("Who wrote this?", d))
}

func (s *Server) reveal(w http.ResponseWriter, r *http.Request) {
	c, d := s.revealCtx(w, r)
	if c == nil {
		return
	}
	v, err := s.Log.Apply(&cmd.RevealAuthor{GroupID: c.g.ID, Kind: d.Kind, ID: d.ID, By: c.u.ID,
		Reason: r.FormValue("reason"), At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "reveal", d) {
		return
	}
	id, _ := v.(int64)
	names, err := s.Store.Handles([]int64{id})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Handle = names[id]
	if d.Handle == "" {
		d.Handle = "[deleted]"
	}
	s.render(w, r, http.StatusOK, "reveal", c.page("Who wrote this?", d))
}

func (s *Server) revealCtx(w http.ResponseWriter, r *http.Request) (*greq, revealData) {
	c := s.modCtx(w, r)
	if c == nil {
		return nil, revealData{}
	}
	d := revealData{Kind: "post"}
	if strings.HasPrefix(r.URL.Path, "/c/") {
		cm := s.loadComment(w, r, c)
		if cm == nil {
			return nil, d
		}
		d.Kind, d.ID, d.PostID = "comment", cm.ID, cm.PostID
	} else {
		p := s.loadPost(w, r, c)
		if p == nil {
			return nil, d
		}
		d.ID, d.PostID = p.ID, p.ID
	}
	return c, d
}

// groupRef finds a group from what an owner typed: its address
// (https://promaster.nfb.group/...), its host, or its short name.
func (s *Server) groupRef(in, primary string) (*store.Group, error) {
	in = strings.ToLower(strings.TrimSpace(in))
	in = strings.TrimPrefix(strings.TrimPrefix(in, "https://"), "http://")
	host, _, _ := strings.Cut(in, "/")
	host, _, _ = strings.Cut(host, ":")
	if g, err := s.Store.GroupByMainHost(host); err != nil || g != nil {
		return g, err
	}
	slug := strings.TrimSuffix(host, "."+primary)
	slug, _, _ = strings.Cut(slug, ".")
	return s.Store.GroupBySlug(slug)
}

type sisterView struct {
	store.Pair
	Group *store.Group
	URL   string
}

// sisterViews resolves a group's pairings for display.
func (s *Server) sisterViews(c *greq, activeOnly bool) ([]sisterView, error) {
	pairs, err := s.Store.Pairs(c.g.ID)
	if err != nil {
		return nil, err
	}
	var out []sisterView
	for _, p := range pairs {
		if activeOnly && p.State != "active" {
			continue
		}
		g, err := s.Store.GroupByID(p.Other)
		if err != nil {
			return nil, err
		}
		if g == nil {
			continue
		}
		// Readers only see sisters they could visit; a hidden group is
		// named only to the owners it's paired with (on Settings).
		if activeOnly && g.Visibility == "hidden" {
			continue
		}
		out = append(out, sisterView{Pair: p, Group: g, URL: s.groupURL(g, c.rt.primary, "/")})
	}
	return out, nil
}

// sisterAction handles the settings page's sister-group forms: propose,
// accept, decline, and end (which also withdraws a proposal).
func (s *Server) sisterAction(w http.ResponseWriter, r *http.Request) {
	c := s.owner(w, r)
	if c == nil {
		return
	}
	at := s.Now().Unix()
	var err error
	switch r.PathValue("action") {
	case "propose":
		var other *store.Group
		other, err = s.groupRef(r.FormValue("group"), c.rt.primary)
		if err == nil && other == nil {
			err = cmd.Invalid("there's no group at that address")
		}
		if err == nil {
			_, err = s.Log.Apply(&cmd.ProposeSister{GroupID: c.g.ID, Other: other.ID, Topics: r.FormValue("topics"), By: c.u.ID, At: at})
		}
	case "accept", "decline":
		other, _ := strconv.ParseInt(r.FormValue("group"), 10, 64)
		_, err = s.Log.Apply(&cmd.AnswerSister{GroupID: c.g.ID, Other: other, Accept: r.PathValue("action") == "accept", By: c.u.ID, At: at})
	case "end":
		other, _ := strconv.ParseInt(r.FormValue("group"), 10, 64)
		_, err = s.Log.Apply(&cmd.EndSister{GroupID: c.g.ID, Other: other, By: c.u.ID, At: at})
	default:
		s.notFound(w, r)
		return
	}
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
	http.Redirect(w, r, "/settings#sisters", http.StatusSeeOther)
}

// unlinkSister takes down a cross-link between a post here and one in a
// sister group (a mod of this group; the other group's mods can do the
// same from their side). It's remembered on both sides.
func (s *Server) unlinkSister(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	og, _ := strconv.ParseInt(r.FormValue("group"), 10, 64)
	op, _ := strconv.ParseInt(r.FormValue("other"), 10, 64)
	if _, err := s.Log.Apply(&cmd.RemoveSisterLink{GroupID: c.g.ID, PostID: p.ID, OtherGroup: og, OtherPost: op,
		By: c.u.ID, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}

// sisterCitable: may this group show notes and cards written from group
// from? The pairing must be active, and the visibility rule must allow
// it. Both come from site.db, so this works on a node that doesn't hold
// the other group.
func (s *Server) sisterCitable(c *greq, from int64) (*store.Group, bool) {
	g, err := s.Store.GroupByID(from)
	if err != nil || g == nil || !g.AIEnabled || !c.st.AIEnabled || !auth.CanCite(g.Visibility) {
		return nil, false
	}
	sisters, err := s.Store.Sisters(c.g.ID)
	if err != nil {
		return nil, false
	}
	for _, p := range sisters {
		if p.Other == from {
			return g, true
		}
	}
	return nil, false
}
