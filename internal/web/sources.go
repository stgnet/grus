package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Outside sources (plan section 5): members add links, mods keep the list
// of sites pages may be read from and approve seeded pages, and anyone can
// ask for a page to be taken down.

type sourceForm struct {
	PostID      int64
	Post        *store.Post
	URL         string
	Description string
	Added       bool
}

func (s *Server) sourceNewForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	f := sourceForm{PostID: postRef(r.URL.Query().Get("post")), Added: r.URL.Query().Get("added") == "1"}
	if !s.writer(w, r, c, "/sources/new?post="+strconv.FormatInt(f.PostID, 10)) {
		return
	}
	if f.PostID != 0 {
		f.Post, _ = s.Store.Post(c.g.ID, f.PostID)
	}
	s.render(w, r, http.StatusOK, "source-new", c.page("Add an outside source", f))
}

// sourceNew adds a link. A mod's link is read at once; a member's is read
// if the mods allow its site, and waits for them otherwise. A description
// means the page is never read (always so for Facebook).
func (s *Server) sourceNew(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	f := sourceForm{PostID: postRef(r.FormValue("post")), URL: strings.TrimSpace(r.FormValue("url")),
		Description: strings.TrimSpace(r.FormValue("description"))}
	if !s.writer(w, r, c, "/sources/new") {
		return
	}
	if f.PostID != 0 {
		if p, err := s.Store.Post(c.g.ID, f.PostID); err != nil || p == nil || p.Status != "visible" {
			f.PostID = 0
		} else {
			f.Post = p
		}
	}
	via := cmd.ViaMember
	if c.mod() {
		via = cmd.ViaMod
	}
	_, err := s.Log.Apply(&cmd.AddSource{GroupID: c.g.ID, SourceID: s.IDs.Next(), URL: f.URL, Summary: f.Description,
		Via: via, AddedBy: c.u.ID, PostID: f.PostID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "source-new", f) {
		return
	}
	if f.PostID != 0 {
		http.Redirect(w, r, fmt.Sprintf("/p/%d#elsewhere", f.PostID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/sources/new?added=1", http.StatusSeeOther)
}

// sourcesFromText (plan section 5, "How sources get in": pasting a link
// is a way of adding an outside source) adds each link in a member's comment or post as an
// outside source on that post, when its site is one the mods allow (links
// to anywhere else are left as plain links; nobody's comment ends up in a
// mod queue just for containing a URL). Failures are ignored: the comment
// itself was saved, and this is extra.
func (s *Server) sourcesFromText(c *greq, postID int64, text string) {
	links := urlRE.FindAllString(text, 5)
	if len(links) == 0 {
		return
	}
	domains, err := s.Store.AllowedDomains(c.g.ID)
	if err != nil || len(domains) == 0 {
		return
	}
	for _, l := range links {
		l = strings.TrimRight(l, ".,);:!?")
		_, site, err := cmd.SourceURL(l)
		if err != nil || !allowedSite(site, domains) {
			continue
		}
		s.Log.Apply(&cmd.AddSource{GroupID: c.g.ID, SourceID: s.IDs.Next(), URL: l, Via: cmd.ViaMember,
			AddedBy: c.u.ID, PostID: postID, At: s.Now().Unix()})
	}
}

func allowedSite(site string, domains []string) bool {
	for _, d := range domains {
		if site == d || strings.HasSuffix(site, "."+d) {
			return true
		}
	}
	return false
}

type removalData struct {
	Source *store.Source
	Post   *store.Post
	Done   bool
}

// sourceRemovalForm and sourceRemoval: anyone can ask for an outside page
// to be taken down, and it is, at once.
func (s *Server) sourceRemovalForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	src := s.loadSource(w, r, c)
	if src == nil {
		return
	}
	s.render(w, r, http.StatusOK, "removal", c.page("Request removal", removalData{Source: src, Done: src.Status == "removed"}))
}

func (s *Server) sourceRemoval(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	src := s.loadSource(w, r, c)
	if src == nil {
		return
	}
	var by int64
	if c.u != nil {
		by = c.u.ID
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if _, err := s.Log.Apply(&cmd.RemoveSource{GroupID: c.g.ID, SourceID: src.ID, By: by, Request: true,
		Reason: reason, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "removal", c.page("Removed", removalData{Source: src, Done: true}))
}

func (s *Server) loadSource(w http.ResponseWriter, r *http.Request, c *greq) *store.Source {
	id, ok := pathID(r)
	if !ok || !c.canRead(nil) {
		s.notFound(w, r)
		return nil
	}
	src, err := s.Store.Source(c.g.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	if src == nil || src.IsList {
		s.notFound(w, r)
		return nil
	}
	return src
}

// archiveRemovalForm and archiveRemoval: the same for an imported archive
// thread (plan section 5): someone who wrote it on Facebook can have it
// taken down here.
func (s *Server) archiveRemovalForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	if p.Origin != "archive" {
		s.notFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "removal", c.page("Request removal", removalData{Post: p, Done: p.Status == "removed"}))
}

func (s *Server) archiveRemoval(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	var by int64
	if c.u != nil {
		by = c.u.ID
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.Log.Apply(&cmd.RequestArchiveRemoval{GroupID: c.g.ID, PostID: p.ID, By: by, Reason: reason, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't remove"}) {
		return
	}
	s.render(w, r, http.StatusOK, "removal", c.page("Removed", removalData{Post: p, Done: true}))
}

// sourceDetach takes a source off a post: a mod, or the post's author.
func (s *Server) sourceDetach(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	src := s.loadSource(w, r, c)
	if src == nil {
		return
	}
	post := postRef(r.FormValue("post"))
	p, err := s.Store.Post(c.g.ID, post)
	if err != nil || p == nil || c.u == nil || (p.UserID != c.u.ID && !c.mod()) {
		s.notFound(w, r)
		return
	}
	if _, err := s.Log.Apply(&cmd.DetachSource{GroupID: c.g.ID, SourceID: src.ID, PostID: post, By: c.u.ID, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", post), http.StatusSeeOther)
}

type modSourcesData struct {
	Queue   []store.Source
	Recent  []store.Source
	Domains []string
}

// modSources is the mods' page for outside sources: the queue (seeded
// pages, links from sites not allowed yet, pages that couldn't be read),
// the allowed sites, the seed form, and recent sources.
func (s *Server) modSources(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	var d modSourcesData
	var err error
	if d.Queue, err = s.Store.SourceQueue(c.g.ID); err == nil {
		if d.Recent, err = s.Store.RecentSources(c.g.ID, 50); err == nil {
			d.Domains, err = s.Store.AllowedDomains(c.g.ID)
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "mod-sources", c.page("Outside sources", d))
}

func (s *Server) modCtx(w http.ResponseWriter, r *http.Request) *greq {
	c := s.group(w, r)
	if c == nil {
		return nil
	}
	if c.u == nil || !c.mod() {
		s.notFound(w, r)
		return nil
	}
	return c
}

// modSourcesAction handles the mods' source buttons: approve (one, or all
// ticked), remove, restore, allow or drop a site, and seed.
func (s *Server) modSourcesAction(w http.ResponseWriter, r *http.Request) {
	c := s.modCtx(w, r)
	if c == nil {
		return
	}
	r.ParseForm()
	at := s.Now().Unix()
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	var err error
	switch r.PathValue("action") {
	case "approve":
		var ids []int64
		for _, v := range r.PostForm["id"] {
			if n, e := strconv.ParseInt(v, 10, 64); e == nil {
				ids = append(ids, n)
			}
		}
		_, err = s.Log.Apply(&cmd.ApproveSources{GroupID: c.g.ID, SourceIDs: ids, By: c.u.ID, At: at})
	case "remove":
		_, err = s.Log.Apply(&cmd.RemoveSource{GroupID: c.g.ID, SourceID: id, By: c.u.ID, At: at})
	case "restore":
		_, err = s.Log.Apply(&cmd.RestoreSource{GroupID: c.g.ID, SourceID: id, By: c.u.ID, At: at})
	case "allow", "disallow":
		_, err = s.Log.Apply(&cmd.AllowDomain{GroupID: c.g.ID, Domain: r.FormValue("domain"),
			Allow: r.PathValue("action") == "allow", By: c.u.ID, At: at})
	case "seed":
		// Pasted links, one per line, and/or a list page whose links on
		// the same site are each read. Everything seeded waits for
		// approval.
		for _, l := range strings.Fields(r.FormValue("links")) {
			if _, e := s.Log.Apply(&cmd.AddSource{GroupID: c.g.ID, SourceID: s.IDs.Next(), URL: l, Via: cmd.ViaSeed,
				AddedBy: c.u.ID, At: at}); e != nil && !cmd.IsInput(e) {
				err = e
				break
			}
		}
		if list := strings.TrimSpace(r.FormValue("list")); list != "" && err == nil {
			_, err = s.Log.Apply(&cmd.AddSource{GroupID: c.g.ID, SourceID: s.IDs.Next(), URL: list, Via: cmd.ViaSeed,
				List: true, AddedBy: c.u.ID, At: at})
		}
	default:
		s.notFound(w, r)
		return
	}
	if err != nil && cmd.IsInput(err) {
		d := modSourcesData{}
		d.Queue, _ = s.Store.SourceQueue(c.g.ID)
		d.Recent, _ = s.Store.RecentSources(c.g.ID, 50)
		d.Domains, _ = s.Store.AllowedDomains(c.g.ID)
		s.commandFailed(w, r, c, err, "mod-sources", d)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/mod/sources", http.StatusSeeOther)
}
