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
	Author    string
	AuthorURL string // their public profile; "" when anonymous
	Images  []store.Image
	CanEdit bool
	NewerID int64 // a later comment replaced this one's advice
	Votes   *voteView
	Helped  bool // this viewer marked it helpful
	CanHelp bool // members, on others' shown comments
}

// voteView is the Keep / Hide vote on a flagged item (plan section 6,
// "Member voting"). Members see the buttons; the server decides whether
// their vote counts (see cmd.Vote for who may vote).
type voteView struct {
	Kind         string // post | comment
	ID           int64
	Keeps, Hides int
	Mine         string // what this viewer voted, "" if nothing
	CanVote      bool
}

// PostView is a post with its photos and comments: the page's main post,
// or one of its Update sections. (Exported only so templates can reach it
// through postData's embedding.)
type PostView struct {
	Post       store.Post
	Author     string
	AuthorURL  string // their public profile; "" when anonymous
	Images     []store.Image
	Threads    []thread
	CanEdit    bool
	CanComment bool
	Votes      *voteView
	Helped     bool
	CanHelp    bool
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
	Sister    bool // from a sister group: removing it is a mod's call
}

// combinedView is a combined note with its numbered sources, so the "[2]"
// in its text points somewhere.
type combinedView struct {
	store.Note
	Sources []citeView
}

type citeView struct {
	N       int
	Title   string
	URL     string
	Site    string // an outside page's site; "" for a thread here
	Date    int64
	Missing bool // gone since the note was written
}

type postData struct {
	PostView
	Updates   []PostView
	TopNotes  []noteView // at the top of the thread
	MoreNotes []noteView // beyond the 5 newest: "N more related posts"
	CanMod    bool
	IsMember  bool
	AllowAnon bool // the group allows anonymous comments
	// M3: how the comments are laid out, and what surrounds them.
	Arr       arrangement
	Plain     bool // ?order=time
	Combined  *combinedView
	FAQ       []store.Entry  // entries this thread is a source of
	Pages     []store.Source // outside pages shown on it ("Elsewhere")
	Topics    []store.Topic
	AllTopics []store.Topic // for the author or a mod to choose from
	CanTopics bool
	Nudges    []nudgeView // for mods: how this thread is arranged
	// Move under an earlier post: the author's own earlier posts to choose
	// from (mods type a link instead).
	MoveChoices []store.Post
	CanMove     bool
	CanLink     bool
	// M6: following the thread (members).
	Following bool
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
	d.Plain = r.URL.Query().Get("order") == "time"
	if err := s.arrangeComments(c, d); err != nil {
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
	d := &postData{PostView: *main, CanMod: c.mod(), IsMember: c.member(), AllowAnon: c.st.AllowAnonymous}
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
	if err := s.surroundings(c, d); err != nil {
		return nil, err
	}
	if c.u != nil && d.IsMember {
		if d.Following, err = s.Store.Following(c.g.ID, p.ID, c.u.ID); err != nil {
			return nil, err
		}
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
	d := &PostView{Post: *p, Author: authorOf(names, p.UserID, p.Anonymous),
		AuthorURL: s.profileURL(c.rt.primary, names, p.UserID, p.Anonymous)}
	d.CanEdit = c.u != nil && p.UserID == c.u.ID
	if d.CanEdit && p.Anonymous {
		d.Author += " (you)" // only the author sees this; everyone else sees just "Anonymous member"
	}
	d.CanComment = !p.Locked && (p.Status == "visible" || p.Status == "flagged")
	if d.Votes, err = s.votes(c, "post", p.ID, p.UserID, p.Status); err != nil {
		return nil, err
	}
	// "Helpful" votes (M6): which of this thread's items the viewer marked.
	helped := map[string]bool{}
	if c.u != nil && c.member() {
		root := p.ID
		if p.ContinuesID != 0 {
			root = p.ContinuesID
		}
		if helped, err = s.Store.MyHelpful(c.g.ID, root, c.u.ID); err != nil {
			return nil, err
		}
	}
	d.Helped = helped[fmt.Sprintf("post:%d", p.ID)]
	d.CanHelp = c.member() && !d.CanEdit && (p.Status == "visible" || p.Status == "flagged")

	byComment := map[int64][]store.Image{}
	for _, im := range images {
		if im.CommentID == 0 {
			d.Images = append(d.Images, im)
		} else {
			byComment[im.CommentID] = append(byComment[im.CommentID], im)
		}
	}
	view := func(cm store.Comment) commentView {
		v := commentView{Comment: cm, Author: authorOf(names, cm.UserID, cm.Anonymous),
			AuthorURL: s.profileURL(c.rt.primary, names, cm.UserID, cm.Anonymous),
			Images: byComment[cm.ID], CanEdit: c.u != nil && cm.UserID == c.u.ID}
		if v.CanEdit && cm.Anonymous {
			v.Author += " (you)"
		}
		// A failed count only loses the buttons, not the page.
		v.Votes, _ = s.votes(c, "comment", cm.ID, cm.UserID, cm.Status)
		v.Helped = helped[fmt.Sprintf("comment:%d", cm.ID)]
		v.CanHelp = c.member() && !v.CanEdit && (cm.Status == "visible" || cm.Status == "flagged")
		return v
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

// votes reads the Keep / Hide tally on a flagged item for members and
// mods; nil when the item isn't flagged or the viewer can't take part.
func (s *Server) votes(c *greq, kind string, id, authorID int64, status string) (*voteView, error) {
	if status != "flagged" || c.u == nil || !(c.member() || c.mod()) {
		return nil, nil
	}
	keeps, hides, mine, err := s.Store.ItemVotes(c.g.ID, kind, id, c.u.ID)
	if err != nil {
		return nil, err
	}
	return &voteView{Kind: kind, ID: id, Keeps: keeps, Hides: hides, Mine: mine,
		CanVote: c.member() && authorID != c.u.ID}, nil
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
	var sisterLinks map[[2]int64]store.SisterLink
	for _, n := range notes {
		if n.Kind != "link" {
			continue
		}
		if n.SourceGroupID != c.g.ID {
			// A note written from a sister group's post, shown only while
			// the pairing holds and the visibility rule allows it (checked
			// here too, not just when it was made, because either group's
			// settings may have changed since).
			g, ok := s.sisterCitable(c, n.SourceGroupID)
			if !ok {
				continue
			}
			if sisterLinks == nil {
				links, err := s.Store.SisterLinks(c.g.ID, d.Post.ID)
				if err != nil {
					return err
				}
				sisterLinks = map[[2]int64]store.SisterLink{}
				for _, l := range links {
					sisterLinks[[2]int64{l.OtherGroup, l.OtherPost}] = l
				}
			}
			l, ok := sisterLinks[[2]int64{n.SourceGroupID, n.SourcePostID}]
			if !ok || l.State != "active" || n.Text == "" {
				continue // not written yet: a sister note has nothing to show without its text
			}
			views = append(views, noteView{Note: n, Title: l.Title, Date: l.Date, Heading: "In the " + g.Name + " group",
				URL: s.groupURL(g, c.rt.primary, fmt.Sprintf("/p/%d", n.SourcePostID)), CanRemove: d.CanMod, Sister: true})
			continue
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

// surroundings gathers what M3 shows around a post: the FAQ entries it's
// part of, the outside pages on it, its combined note, and its topics.
func (s *Server) surroundings(c *greq, d *postData) error {
	var err error
	g, id := c.g.ID, d.Post.ID
	if d.FAQ, err = s.Store.EntriesForPost(g, id); err != nil {
		return err
	}
	pages, err := s.Store.SourcesForPost(g, id)
	if err != nil {
		return err
	}
	for _, pg := range pages {
		if pg.Shown() {
			d.Pages = append(d.Pages, pg)
		}
	}
	if d.Topics, _, err = s.Store.PostTopics(g, id); err != nil {
		return err
	}
	shown := d.Post.Status == "visible" || d.Post.Status == "flagged"
	d.CanTopics = shown && (d.CanEdit || d.CanMod) && d.Post.ContinuesID == 0
	if d.CanTopics {
		tree, err := s.Store.Topics(g)
		if err != nil {
			return err
		}
		d.AllTopics = store.FlatTopics(tree)
	}
	notes, err := s.Store.Notes(g, id)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.Kind != "combined" || n.Text == "" {
			continue
		}
		srcs, err := s.Store.NoteSources(g, n.ID)
		if err != nil {
			return err
		}
		cv := &combinedView{Note: n}
		for i, src := range srcs {
			cite := citeView{N: i + 1, Missing: true}
			if src.SourceID != 0 {
				if pg, err := s.Store.Source(g, src.SourceID); err == nil && pg != nil && pg.Shown() {
					cite = citeView{N: i + 1, Title: pg.Title, URL: pg.URL, Site: pg.Site, Date: pg.PublishedAt}
				}
			} else if p, err := s.Store.Post(g, src.PostID); err == nil && p != nil &&
				c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) && (p.Status == "visible" || p.Status == "flagged") {
				cite = citeView{N: i + 1, Title: p.Title, URL: fmt.Sprintf("/p/%d", p.ID), Date: p.CreatedAt}
			}
			cv.Sources = append(cv.Sources, cite)
		}
		d.Combined = cv
	}
	return nil
}

// arrangeComments lays out the main post's comments (see arrange.go).
func (s *Server) arrangeComments(c *greq, d *postData) error {
	notes, err := s.Store.Notes(c.g.ID, d.Post.ID)
	if err != nil {
		return err
	}
	var summary *store.Note
	covers := map[int64]bool{}
	for _, n := range notes {
		if n.Kind == "summary" && n.Text != "" {
			n := n
			summary = &n
			srcs, err := s.Store.NoteSources(c.g.ID, n.ID)
			if err != nil {
				return err
			}
			for _, src := range srcs {
				if src.Covers {
					covers[src.CommentID] = true
				}
			}
		}
	}
	nudges, err := s.Store.Nudges(c.g.ID, d.Post.ID)
	if err != nil {
		return err
	}
	var threadNudges []store.Nudge
	for _, n := range nudges {
		if n.Kind != "feed_weight" {
			threadNudges = append(threadNudges, n)
		}
	}
	d.Arr = arrange(d.Threads, summary, covers, threadNudges, d.Plain)
	if d.CanMod {
		d.Nudges = describeNudges(nudges)
	}
	return nil
}
