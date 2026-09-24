package web

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/store"
)

// The post page: the post, its Update sections, its comments, and the
// notes the system placed in the thread (plan section 2).

// thread is one top-level comment and its replies, as the post page shows.
type thread struct {
	Comment  commentView
	Replies  []commentView
	Readable bool       // false: a placeholder kept so its replies have a parent
	Notes    []noteView // notes placed after this part of the conversation
}

type commentView struct {
	store.Comment
	Author  string
	Images  []store.Image
	CanEdit bool
}

// PostView is a post with its photos and comments: the page's main post,
// or one of its Update sections. (Exported only so templates can reach it
// through postData's embedding.)
type PostView struct {
	Post       store.Post
	Author     string
	Images     []store.Image
	Threads    []thread
	CanEdit    bool
	CanComment bool
}

// noteView is a note as a related-post card. It has no author and no
// avatar and is styled unlike a comment, so it never looks like a member
// talking (plan section 1).
type noteView struct {
	store.Note
	Heading   string // "Newer information", "Earlier discussion"
	Title     string // of the post it points at
	Date      int64
	URL       string
	CanRemove bool
}

type postData struct {
	PostView
	Updates   []PostView
	TopNotes  []noteView // at the top of the thread
	MoreNotes []noteView // beyond the 5 newest: "N more related posts"
	CanMod    bool
	IsMember  bool
	// Move under an earlier post: the author's own earlier posts to choose
	// from (mods type a link instead).
	MoveChoices []store.Post
	CanMove     bool
	CanLink     bool
}

// maxShownNotes: a thread shows its 5 newest link notes in place; older
// ones collapse into "N more related posts" at the end.
const maxShownNotes = 5

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
	if p.ContinuesID != 0 {
		// It's an Update inside another thread now; its own address keeps
		// working by going to its place there.
		http.Redirect(w, r, fmt.Sprintf("/p/%d#u%d", p.ContinuesID, p.ID), http.StatusSeeOther)
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
	main, err := s.postView(c, p)
	if err != nil {
		return nil, err
	}
	d := &postData{PostView: *main, CanMod: c.mod(), IsMember: c.member()}
	ups, err := s.Store.Updates(c.g.ID, p.ID)
	if err != nil {
		return nil, err
	}
	for _, u := range ups {
		if !c.canRead(&auth.Item{AuthorID: u.UserID, Status: u.Status}) {
			continue
		}
		v, err := s.postView(c, &u)
		if err != nil {
			return nil, err
		}
		d.Updates = append(d.Updates, *v)
	}
	if err := s.placeNotes(c, d); err != nil {
		return nil, err
	}
	shown := p.Status == "visible" || p.Status == "flagged"
	d.CanLink = shown && (d.CanEdit || d.CanMod)
	if shown && len(d.Updates) == 0 && (d.CanEdit || d.CanMod) {
		d.CanMove = true
		if d.CanEdit && c.u != nil {
			if d.MoveChoices, err = s.Store.PostsByUser(c.g.ID, c.u.ID, p.ID, 20); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}

// postView gathers one post's photos and comments.
func (s *Server) postView(c *greq, p *store.Post) (*PostView, error) {
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
	d := &PostView{Post: *p, Author: authorOf(names, p.UserID, p.Anonymous)}
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

// placeNotes puts the thread's link notes where each connection was made:
// after the comment that was latest at the time (or at the top). Only the
// 5 newest show in place. A note whose other post is gone disappears with
// it, whatever the note's own state.
func (s *Server) placeNotes(c *greq, d *postData) error {
	notes, err := s.Store.Notes(c.g.ID, d.Post.ID)
	if err != nil {
		return err
	}
	var views []noteView
	for _, n := range notes {
		if n.Kind != "link" || n.SourceGroupID != c.g.ID {
			continue // other kinds and cross-group notes arrive with M3 and M4
		}
		other, err := s.Store.Post(c.g.ID, n.SourcePostID)
		if err != nil {
			return err
		}
		if other == nil || (other.Status != "visible" && other.Status != "flagged") {
			continue
		}
		v := noteView{Note: n, Title: other.Title, Date: other.CreatedAt, URL: fmt.Sprintf("/p/%d", other.ID),
			Heading: "Earlier discussion", CanRemove: d.CanMod || d.CanEdit}
		// Ids grow with time, so they break a tie between posts made in
		// the same second.
		if other.CreatedAt > d.Post.CreatedAt || other.CreatedAt == d.Post.CreatedAt && other.ID > d.Post.ID {
			v.Heading = "Newer information"
		}
		views = append(views, v)
	}
	// Newest first to choose which show; the rest go to the end.
	sort.SliceStable(views, func(i, j int) bool { return views[i].ID > views[j].ID })
	if len(views) > maxShownNotes {
		d.MoreNotes = views[maxShownNotes:]
		views = views[:maxShownNotes]
	}
	// Map every comment (replies included) to its top-level thread.
	where := map[int64]int{}
	for i, t := range d.Threads {
		where[t.Comment.ID] = i
		for _, r := range t.Replies {
			where[r.ID] = i
		}
	}
	for i := len(views) - 1; i >= 0; i-- { // back to oldest first
		v := views[i]
		if t, ok := where[v.AfterCommentID]; ok && v.AfterCommentID != 0 {
			d.Threads[t].Notes = append(d.Threads[t].Notes, v)
		} else {
			d.TopNotes = append(d.TopNotes, v)
		}
	}
	return nil
}
