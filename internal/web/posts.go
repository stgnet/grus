package web

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/img"
	"github.com/stgnet/grus/internal/store"
)

// Posting and discussion (plan section 2). Every handler starts from
// s.group (which applies the group-level read rule), checks the item-level
// rule for what it touches, and writes only by submitting a command.

const (
	maxUpload      = 20 << 20  // one photo, before processing
	maxPostRequest = 120 << 20 // a post with its photos
)

type submitData struct {
	Title, Body string
}

func (s *Server) submitForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil || !s.writer(w, r, c, "/submit") {
		return
	}
	s.render(w, r, http.StatusOK, "submit", c.page("New post", submitData{}))
}

// submitPost creates a post. Photos are processed and stored first (and
// pushed to the other nodes), then the post command refers to them by hash.
func (s *Server) submitPost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil || !s.writer(w, r, c, "/submit") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPostRequest)
	if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		s.render(w, r, http.StatusBadRequest, "submit", c.errPage("New post", "That upload was too large. Try fewer or smaller photos.", submitData{}))
		return
	}
	d := submitData{Title: strings.TrimSpace(r.FormValue("title")), Body: strings.TrimSpace(r.FormValue("body"))}
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["photos"]
	}
	if len(files) > cmd.MaxImagesPerPost {
		s.render(w, r, http.StatusBadRequest, "submit", c.errPage("New post", fmt.Sprintf("A post can have up to %d photos.", cmd.MaxImagesPerPost), d))
		return
	}
	images, err := s.storePhotos(files)
	if err != nil {
		s.render(w, r, http.StatusBadRequest, "submit", c.errPage("New post", err.Error(), d))
		return
	}
	id := s.IDs.Next()
	_, err = s.Log.Apply(&cmd.CreatePost{GroupID: c.g.ID, PostID: id, UserID: c.u.ID,
		Title: d.Title, Body: d.Body, Images: images, At: s.Now().Unix()})
	if cmd.IsInput(err) {
		s.render(w, r, http.StatusBadRequest, "submit", c.errPage("New post", capitalize(cmdMessage(err))+".", d))
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", id), http.StatusSeeOther)
}

func (c *greq) errPage(title, msg string, data any) *page {
	p := c.page(title, data)
	p.Error = msg
	return p
}

// storePhotos processes uploaded photos (re-encoded, metadata stripped,
// resized), stores them, and returns them ready for a command.
func (s *Server) storePhotos(files []*multipart.FileHeader) ([]cmd.Image, error) {
	var out []cmd.Image
	for _, fh := range files {
		if fh.Size == 0 {
			continue // an empty file input
		}
		f, err := fh.Open()
		if err != nil {
			return nil, err
		}
		data, err := img.ReadLimited(f, maxUpload)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s is too large (20 MB at most)", fh.Filename)
		}
		res, err := img.Process(data)
		if err != nil {
			return nil, fmt.Errorf("%s isn't a photo we can read (JPEG, PNG, GIF or WebP)", fh.Filename)
		}
		hash, err := s.Blobs.Put(res.Full, res.Thumb)
		if err != nil {
			return nil, err
		}
		if s.PushBlob != nil {
			s.PushBlob(hash)
		}
		out = append(out, cmd.Image{ID: s.IDs.Next(), Hash: hash, Width: res.Width, Height: res.Height, Bytes: len(res.Full)})
	}
	return out, nil
}

// thread is one top-level comment and its replies, as the post page shows.
type thread struct {
	Comment  commentView
	Replies  []commentView
	Readable bool // false: a placeholder kept so its replies have a parent
}

type commentView struct {
	store.Comment
	Author  string
	Images  []store.Image
	CanEdit bool
}

type postData struct {
	Post       store.Post
	Author     string
	Images     []store.Image
	Threads    []thread
	CanEdit    bool
	CanMod     bool
	CanComment bool
	IsMember   bool
}

// loadPost reads a post and applies the read rule to it. It writes a 404
// and returns nil when the viewer may not see it.
func (s *Server) loadPost(w http.ResponseWriter, r *http.Request, c *greq) *store.Post {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return nil
	}
	p, err := s.Store.Post(c.g.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	if p == nil || !c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) {
		s.render(w, r, http.StatusNotFound, "notfound", c.page("Not found", nil))
		return nil
	}
	return p
}

func (s *Server) postPage(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	if !c.canRead(nil) {
		// A private group: the name and the sign-in box, not the post.
		s.render(w, r, http.StatusOK, "group", c.page(c.g.Name, feedData{Settings: c.st}))
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	d, err := s.postData(c, p)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "post", c.page(p.Title, d))
}

func (s *Server) postData(c *greq, p *store.Post) (*postData, error) {
	comments, err := s.Store.Comments(c.g.ID, p.ID)
	if err != nil {
		return nil, err
	}
	images, err := s.Store.Images(c.g.ID, p.ID)
	if err != nil {
		return nil, err
	}
	names, err := s.authorNames([]store.Post{*p}, comments)
	if err != nil {
		return nil, err
	}
	d := &postData{Post: *p, Author: authorOf(names, p.UserID, p.Anonymous), CanMod: c.mod(), IsMember: c.member()}
	d.CanEdit = c.u != nil && p.UserID == c.u.ID
	d.CanComment = !p.Locked && (p.Status == "visible" || p.Status == "flagged")

	byComment := map[int64][]store.Image{}
	for _, im := range images {
		if im.CommentID == 0 {
			d.Images = append(d.Images, im)
		} else {
			byComment[im.CommentID] = append(byComment[im.CommentID], im)
		}
	}
	view := func(cm store.Comment) commentView {
		return commentView{Comment: cm, Author: authorOf(names, cm.UserID, cm.Anonymous),
			Images: byComment[cm.ID], CanEdit: c.u != nil && cm.UserID == c.u.ID}
	}
	readable := func(cm store.Comment) bool {
		return c.canRead(&auth.Item{AuthorID: cm.UserID, Status: cm.Status})
	}

	// Top-level comments in order, each with its readable replies.
	index := map[int64]int{}
	for _, cm := range comments {
		if cm.ParentID == 0 {
			index[cm.ID] = len(d.Threads)
			d.Threads = append(d.Threads, thread{Comment: view(cm), Readable: readable(cm)})
		}
	}
	for _, cm := range comments {
		if cm.ParentID == 0 || !readable(cm) {
			continue
		}
		if i, ok := index[cm.ParentID]; ok {
			d.Threads[i].Replies = append(d.Threads[i].Replies, view(cm))
		}
	}
	// Drop unreadable comments unless their replies need them as a header.
	kept := d.Threads[:0]
	for _, t := range d.Threads {
		if t.Readable || len(t.Replies) > 0 {
			kept = append(kept, t)
		}
	}
	d.Threads = kept
	return d, nil
}

func (s *Server) editPostForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	if c.u == nil || p.UserID != c.u.ID {
		s.notFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "edit", c.page("Edit post", editData{Action: fmt.Sprintf("/p/%d/edit", p.ID),
		Back: fmt.Sprintf("/p/%d", p.ID), IsPost: true, Title: p.Title, Body: p.Body}))
}

type editData struct {
	Action, Back string
	IsPost       bool
	Title, Body  string
}

func (s *Server) editPost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	if c.u == nil || p.UserID != c.u.ID {
		s.notFound(w, r)
		return
	}
	d := editData{Action: fmt.Sprintf("/p/%d/edit", p.ID), Back: fmt.Sprintf("/p/%d", p.ID), IsPost: true,
		Title: strings.TrimSpace(r.FormValue("title")), Body: strings.TrimSpace(r.FormValue("body"))}
	_, err := s.Log.Apply(&cmd.EditPost{GroupID: c.g.ID, PostID: p.ID, EditorID: c.u.ID, Title: d.Title, Body: d.Body, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "edit", d) {
		return
	}
	http.Redirect(w, r, d.Back, http.StatusSeeOther)
}

// commandFailed handles a command error: the person's own mistakes are
// shown on the form (tmpl) again; anything else is a server error. It
// returns true when it has written the response.
func (s *Server) commandFailed(w http.ResponseWriter, r *http.Request, c *greq, err error, tmpl string, data any) bool {
	if err == nil {
		return false
	}
	if cmd.IsInput(err) {
		s.render(w, r, http.StatusBadRequest, tmpl, c.errPage("Try again", capitalize(cmdMessage(err))+".", data))
		return true
	}
	s.serverError(w, r, err)
	return true
}

// deletePost: the author deletes (hidden now, purged in 30 days), or a mod
// removes (kept 90 days for appeals).
func (s *Server) deletePost(w http.ResponseWriter, r *http.Request) {
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
	_, err := s.Log.Apply(&cmd.SoftDelete{GroupID: c.g.ID, Kind: "post", ID: p.ID, By: c.u.ID, ByMod: !isAuthor,
		Reason: strings.TrimSpace(r.FormValue("reason")), At: s.Now().Unix()})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if isAuthor {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}

func (s *Server) restorePost(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	p := s.loadPost(w, r, c)
	if p == nil {
		return
	}
	if !c.mod() {
		s.notFound(w, r)
		return
	}
	if _, err := s.Log.Apply(&cmd.Restore{GroupID: c.g.ID, Kind: "post", ID: p.ID, By: c.u.ID, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}

// addComment adds a comment or reply, with at most one photo.
func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		s.render(w, r, http.StatusBadRequest, "message", c.page("Too large", message{Title: "Too large", Text: "That photo was too large."}))
		return
	}
	var image *cmd.Image
	if r.MultipartForm != nil && len(r.MultipartForm.File["photo"]) > 0 {
		imgs, err := s.storePhotos(r.MultipartForm.File["photo"][:1])
		if err != nil {
			s.render(w, r, http.StatusBadRequest, "message", c.page("Photo problem", message{Title: "Photo problem", Text: err.Error()}))
			return
		}
		if len(imgs) == 1 {
			image = &imgs[0]
		}
	}
	var parent int64
	fmt.Sscan(r.FormValue("parent"), &parent)
	id := s.IDs.Next()
	_, err := s.Log.Apply(&cmd.CreateComment{GroupID: c.g.ID, CommentID: id, PostID: p.ID, ParentID: parent,
		UserID: c.u.ID, Body: strings.TrimSpace(r.FormValue("body")), Image: image, At: s.Now().Unix()})
	if err != nil {
		if cmd.IsInput(err) {
			s.render(w, r, http.StatusBadRequest, "message", c.page("Can't comment",
				message{Title: "Can't comment", Text: capitalize(cmdMessage(err)) + "."}))
			return
		}
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s#c%d", back, id), http.StatusSeeOther)
}

// loadComment reads a comment and applies the read rule to it and its post.
func (s *Server) loadComment(w http.ResponseWriter, r *http.Request, c *greq) *store.Comment {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return nil
	}
	cm, err := s.Store.Comment(c.g.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	if cm == nil || !c.canRead(&auth.Item{AuthorID: cm.UserID, Status: cm.Status}) {
		s.notFound(w, r)
		return nil
	}
	p, err := s.Store.Post(c.g.ID, cm.PostID)
	if err != nil || p == nil || !c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) {
		s.notFound(w, r)
		return nil
	}
	return cm
}

func (s *Server) editCommentForm(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	cm := s.loadComment(w, r, c)
	if cm == nil {
		return
	}
	if c.u == nil || cm.UserID != c.u.ID {
		s.notFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "edit", c.page("Edit comment", editData{Action: fmt.Sprintf("/c/%d/edit", cm.ID),
		Back: fmt.Sprintf("/p/%d#c%d", cm.PostID, cm.ID), Body: cm.Body}))
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	cm := s.loadComment(w, r, c)
	if cm == nil {
		return
	}
	if c.u == nil || cm.UserID != c.u.ID {
		s.notFound(w, r)
		return
	}
	d := editData{Action: fmt.Sprintf("/c/%d/edit", cm.ID), Back: fmt.Sprintf("/p/%d#c%d", cm.PostID, cm.ID),
		Body: strings.TrimSpace(r.FormValue("body"))}
	_, err := s.Log.Apply(&cmd.EditComment{GroupID: c.g.ID, CommentID: cm.ID, EditorID: c.u.ID, Body: d.Body, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "edit", d) {
		return
	}
	http.Redirect(w, r, d.Back, http.StatusSeeOther)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	cm := s.loadComment(w, r, c)
	if cm == nil {
		return
	}
	isAuthor := c.u != nil && cm.UserID == c.u.ID
	if !isAuthor && !c.mod() {
		s.notFound(w, r)
		return
	}
	_, err := s.Log.Apply(&cmd.SoftDelete{GroupID: c.g.ID, Kind: "comment", ID: cm.ID, By: c.u.ID, ByMod: !isAuthor,
		Reason: strings.TrimSpace(r.FormValue("reason")), At: s.Now().Unix()})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", cm.PostID), http.StatusSeeOther)
}

func (s *Server) restoreComment(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	cm := s.loadComment(w, r, c)
	if cm == nil {
		return
	}
	if !c.mod() {
		s.notFound(w, r)
		return
	}
	if _, err := s.Log.Apply(&cmd.Restore{GroupID: c.g.ID, Kind: "comment", ID: cm.ID, By: c.u.ID, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d#c%d", cm.PostID, cm.ID), http.StatusSeeOther)
}

// serveImage sends a photo after the access check: it may be seen by anyone
// who can read at least one post or comment it's on. (Filenames are hashes,
// which are hard to guess, but that's defense in depth, not the control.)
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	hash := r.PathValue("hash")
	thumb := strings.HasSuffix(r.URL.Path, "/t")
	uses, err := s.Store.ImageUses(c.g.ID, hash)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	allowed := false
	for _, u := range uses {
		p, err := s.Store.Post(c.g.ID, u.PostID)
		if err != nil || p == nil || !c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) {
			continue
		}
		if u.CommentID != 0 {
			cm, err := s.Store.Comment(c.g.ID, u.CommentID)
			if err != nil || cm == nil || !c.canRead(&auth.Item{AuthorID: cm.UserID, Status: cm.Status}) {
				continue
			}
		}
		allowed = true
		break
	}
	if !allowed {
		http.NotFound(w, r)
		return
	}
	f, err := s.Blobs.Open(hash, thumb)
	if err != nil {
		// Not on this node yet (it's still being copied here).
		log.Printf("image %s missing locally: %v", hash, err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// A hash's bytes never change, so browsers may keep it forever. Photos
	// in public groups may also be cached by shared caches; others only by
	// the reader's own browser.
	cc := "private, max-age=31536000, immutable"
	if c.st.Visibility == "public" {
		cc = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Cache-Control", cc)
	w.Header().Set("Content-Type", "image/jpeg")
	io.Copy(w, f)
}
