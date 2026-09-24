package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
)

// Linking posts by hand, and continuations (plan section 2). The AI links
// posts on its own through the check job; these are the manual paths:
// the author or a mod links two posts, removes a link, or moves a "part 2"
// post under the earlier one.

var postRefRE = regexp.MustCompile(`/p/(\d+)`)

// postRef reads a post number from what someone typed: a link to the post
// (on any of our addresses), or just its number.
func postRef(s string) int64 {
	s = strings.TrimSpace(s)
	if m := postRefRE.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// linkPost links this post to another one in the group. Both get a note.
func (s *Server) linkPost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	isAuthor := c.u != nil && p.UserID == c.u.ID
	if !isAuthor && !c.mod() {
		s.notFound(w, r)
		return
	}
	other := postRef(r.FormValue("other"))
	back := fmt.Sprintf("/p/%d", p.ID)
	if o, err := s.Store.Post(c.g.ID, other); err != nil || o == nil {
		s.render(w, r, http.StatusBadRequest, "message", c.page("Can't link", message{Title: "Can't link",
			Text: "That isn't a post in this group. Paste the other post's link, or its number."}))
		return
	}
	source := "author"
	if !isAuthor {
		source = "mod"
	}
	_, err := s.Log.Apply(&cmd.AddLink{GroupID: c.g.ID, PostA: p.ID, PostB: other, Source: source, By: c.u.ID,
		NoteA: s.IDs.Next(), NoteB: s.IDs.Next(), CombinedA: s.IDs.Next(), CombinedB: s.IDs.Next(), At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't link"}) {
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// unlinkPost removes a link: a mod, or the author of the post it shows on.
// The pair is remembered, so it isn't linked again automatically.
func (s *Server) unlinkPost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	isAuthor := c.u != nil && p.UserID == c.u.ID
	if !isAuthor && !c.mod() {
		s.notFound(w, r)
		return
	}
	_, err := s.Log.Apply(&cmd.RemoveLink{GroupID: c.g.ID, PostA: p.ID, PostB: postRef(r.FormValue("other")),
		By: c.u.ID, ByMod: !isAuthor, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't remove", Text: "That link is already gone."}) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}

// movePost makes this post an Update under an earlier one. The author may
// move their own post under their own earlier post; a mod may move any.
func (s *Server) movePost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	isAuthor := c.u != nil && p.UserID == c.u.ID
	if !isAuthor && !c.mod() {
		s.notFound(w, r)
		return
	}
	under := postRef(r.FormValue("under"))
	if !c.mod() {
		if o, err := s.Store.Post(c.g.ID, under); err != nil || o == nil || o.UserID != c.u.ID {
			s.render(w, r, http.StatusBadRequest, "message", c.page("Can't move", message{Title: "Can't move",
				Text: "You can move a post under one of your own earlier posts. A moderator can move others."}))
			return
		}
	}
	_, err := s.Log.Apply(&cmd.MoveUnder{GroupID: c.g.ID, PostID: p.ID, UnderID: under, By: c.u.ID, ByMod: !isAuthor, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't move"}) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d#u%d", under, p.ID), http.StatusSeeOther)
}

// moveOutPost undoes a move.
func (s *Server) moveOutPost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	isAuthor := c.u != nil && p.UserID == c.u.ID
	if !isAuthor && !c.mod() {
		s.notFound(w, r)
		return
	}
	if _, err := s.Log.Apply(&cmd.MoveOut{GroupID: c.g.ID, PostID: p.ID, By: c.u.ID, ByMod: !isAuthor, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}
