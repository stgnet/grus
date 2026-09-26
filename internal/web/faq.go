package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The group FAQ pages (plan section 4), and the root FAQ on the bare
// domain (any of them), which is the same code over a reserved group file
// (cmd.RootGroupID) whose editors are the site's operators.

// faqCtx is the request context for FAQ pages: the group's, or on the bare
// domain, the root FAQ's. It writes a 404 and returns nil when the viewer
// can't read this FAQ.
func (s *Server) faqCtx(w http.ResponseWriter, r *http.Request) *greq {
	rt := routeOf(r)
	if rt.group == nil {
		u := s.user(r)
		c := &greq{rt: rt, root: true, u: u,
			g:  &store.Group{ID: cmd.RootGroupID, Name: rt.domain},
			st: &store.Settings{Name: rt.domain, Visibility: "public"}}
		if u != nil {
			c.v = auth.Viewer{UserID: u.ID, Operator: u.IsOperator}
		}
		return c
	}
	c := s.group(w, r)
	if c == nil {
		return nil
	}
	if !auth.CanReadFAQ(c.v, c.st.Visibility, c.st.PublicFAQ) {
		s.render(w, r, http.StatusOK, "group", c.page(c.g.Name, feedData{Settings: c.st}))
		return nil
	}
	return c
}

// faqMod is faqCtx for mod-only actions: the group's mods, or on the root
// FAQ, the operators.
func (s *Server) faqMod(w http.ResponseWriter, r *http.Request) *greq {
	c := s.faqCtx(w, r)
	if c == nil {
		return nil
	}
	if c.u == nil || !c.mod() {
		s.notFound(w, r)
		return nil
	}
	return c
}

type faqTopicView struct {
	Topic    store.Topic
	Entries  []store.Entry
	Children []faqTopicView
}

type rootGroupCard struct {
	Name, Description, URL string
	Topics                 []string
}

type faqData struct {
	Root        bool
	CanMod      bool
	Topics      []faqTopicView
	Other       []store.Entry // entries whose topic is gone
	AllTopics   []store.Topic
	Groups      []rootGroupCard // the root FAQ's "what groups are there"
	Suggestions int             // locked entries with a rewrite waiting (mods)
	Empty       bool
	Sisters     []faqSister // sister groups' FAQs ("for chassis issues, see the ProMaster FAQ")
}

type faqSister struct {
	Name, URL, Topics string
}

// faqPage is the FAQ: the topic tree with its entries.
func (s *Server) faqPage(w http.ResponseWriter, r *http.Request) {
	c := s.faqCtx(w, r)
	if c == nil {
		return
	}
	tree, err := s.Store.Topics(c.g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	entries, err := s.Store.Entries(c.g.ID, c.mod())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := faqData{Root: c.root, CanMod: c.mod(), AllTopics: store.FlatTopics(tree), Empty: len(entries) == 0}
	byTopic := map[int64][]store.Entry{}
	for _, e := range entries {
		byTopic[e.TopicID] = append(byTopic[e.TopicID], e)
		if e.Suggestion != "" {
			d.Suggestions++
		}
	}
	placed := map[int64]bool{}
	for _, t := range tree {
		tv := faqTopicView{Topic: t, Entries: byTopic[t.ID]}
		placed[t.ID] = true
		for _, ch := range t.Children {
			placed[ch.ID] = true
			if len(byTopic[ch.ID]) > 0 || d.CanMod {
				tv.Children = append(tv.Children, faqTopicView{Topic: ch, Entries: byTopic[ch.ID]})
			}
		}
		if len(tv.Entries) > 0 || len(tv.Children) > 0 || d.CanMod {
			d.Topics = append(d.Topics, tv)
		}
	}
	for _, e := range entries {
		if !placed[e.TopicID] {
			d.Other = append(d.Other, e)
		}
	}
	if c.root {
		if d.Groups, err = s.rootGroups(r, c); err != nil {
			s.serverError(w, r, err)
			return
		}
	} else if pairs, err := s.Store.Sisters(c.g.ID); err == nil {
		// Only sisters whose FAQ everyone can read: the same rule as
		// notes, so a public FAQ never points into a private group.
		for _, p := range pairs {
			if g, ok := s.sisterCitable(c, p.Other); ok {
				d.Sisters = append(d.Sisters, faqSister{Name: g.Name, URL: s.groupURL(g, c.rt.at, "/faq"), Topics: p.Topics})
			}
		}
	}
	title := "FAQ"
	if !c.root {
		title = c.g.Name + " FAQ"
	}
	s.render(w, r, http.StatusOK, "faq", c.page(title, d))
}

// rootGroups is the root FAQ's list of groups: every group this visitor can
// see, with its description and the top of its own FAQ outline, each
// linking into that group's FAQ (plan section 4, "The root FAQ").
func (s *Server) rootGroups(r *http.Request, c *greq) ([]rootGroupCard, error) {
	groups, err := s.Store.Groups()
	if err != nil {
		return nil, err
	}
	var out []rootGroupCard
	for i := range groups {
		g := &groups[i]
		st, err := s.Store.GroupSettings(g.ID)
		if err != nil || st == nil {
			continue
		}
		v, err := s.viewer(c.u, g.ID)
		if err != nil || !auth.CanSeeGroup(v, st.Visibility) {
			continue
		}
		card := rootGroupCard{Name: g.Name, Description: st.Description, URL: s.groupURL(g, c.rt.at, "/faq")}
		if auth.CanReadFAQ(v, st.Visibility, st.PublicFAQ) {
			tree, err := s.Store.Topics(g.ID)
			if err != nil {
				return nil, err
			}
			for _, t := range tree {
				if len(card.Topics) < 6 {
					card.Topics = append(card.Topics, t.Title)
				}
			}
		}
		out = append(out, card)
	}
	return out, nil
}

type entryData struct {
	Entry       *store.Entry
	Topic       *store.Topic
	Posts       []store.Post
	Hidden      int // source threads this reader can't open (a private group's preview)
	Pages       []store.Source
	MorePages   bool // the group's own threads cover it well: pages go under "more sources"
	Comments    []faqCommentView
	CanComment  bool
	CanMod      bool
	Root        bool
	UpdatedByAI bool
}

type faqCommentView struct {
	store.FAQComment
	Author    string
	CanDelete bool
}

func (s *Server) loadEntry(w http.ResponseWriter, r *http.Request, c *greq) *store.Entry {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return nil
	}
	e, err := s.Store.Entry(c.g.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	if e == nil || e.Status != "active" && !c.mod() {
		s.render(w, r, http.StatusNotFound, "notfound", c.page("Not found", nil))
		return nil
	}
	return e
}

// entryPage is one entry: the answer, the threads it's based on, outside
// pages, and members' comments.
func (s *Server) entryPage(w http.ResponseWriter, r *http.Request) {
	c := s.faqCtx(w, r)
	if c == nil {
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	d, err := s.entryData(c, e)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "faq-entry", c.page(e.Question, d))
}

func (s *Server) entryData(c *greq, e *store.Entry) (*entryData, error) {
	d := &entryData{Entry: e, CanMod: c.mod(), Root: c.root, UpdatedByAI: e.UpdatedBy == 0,
		CanComment: !c.root && c.member()}
	var err error
	if d.Topic, err = s.Store.Topic(c.g.ID, e.TopicID); err != nil {
		return nil, err
	}
	posts, err := s.Store.EntryPosts(c.g.ID, e.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range posts {
		if p.Status != "visible" && p.Status != "flagged" {
			continue
		}
		if c.canRead(&auth.Item{AuthorID: p.UserID, Status: p.Status}) {
			d.Posts = append(d.Posts, p)
		} else {
			d.Hidden++
		}
	}
	pages, err := s.Store.SourcesForEntry(c.g.ID, e.ID)
	if err != nil {
		return nil, err
	}
	for _, pg := range pages {
		if pg.Shown() {
			d.Pages = append(d.Pages, pg)
		}
	}
	d.MorePages = len(d.Posts)+d.Hidden >= 3
	if !c.canRead(nil) {
		// The public preview of a private group's FAQ: the entry, not the
		// members' comments on it.
		return d, nil
	}
	comments, err := s.Store.EntryComments(c.g.ID, e.ID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, cm := range comments {
		ids = append(ids, cm.UserID)
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		return nil, err
	}
	for _, cm := range comments {
		d.Comments = append(d.Comments, faqCommentView{FAQComment: cm, Author: authorOf(names, cm.UserID, false),
			CanDelete: c.u != nil && (cm.UserID == c.u.ID || c.mod())})
	}
	return d, nil
}

type entryForm struct {
	Entry     *store.Entry // nil for a new entry
	Question  string
	Answer    string
	TopicID   int64
	Locked    bool
	Hidden    bool
	AllTopics []store.Topic
	Root      bool
}

// entryNewForm and entryNew: a mod writes an entry by hand, for instance to
// seed the FAQ before a group has any history.
func (s *Server) entryNewForm(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	f, err := s.entryForm(c, nil)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f.TopicID, _ = strconv.ParseInt(r.URL.Query().Get("topic"), 10, 64)
	s.render(w, r, http.StatusOK, "faq-edit", c.page("New FAQ entry", f))
}

func (s *Server) entryForm(c *greq, e *store.Entry) (*entryForm, error) {
	tree, err := s.Store.Topics(c.g.ID)
	if err != nil {
		return nil, err
	}
	f := &entryForm{Entry: e, AllTopics: store.FlatTopics(tree), Root: c.root}
	if e != nil {
		f.Question, f.Answer, f.TopicID, f.Locked, f.Hidden = e.Question, e.Answer, e.TopicID, e.Locked, e.Status == "hidden"
	}
	return f, nil
}

func (s *Server) entryNew(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	f, err := s.entryForm(c, nil)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f.Question, f.Answer = strings.TrimSpace(r.FormValue("question")), strings.TrimSpace(r.FormValue("answer"))
	f.TopicID, _ = strconv.ParseInt(r.FormValue("topic"), 10, 64)
	add := &cmd.CreateFAQEntry{GroupID: c.g.ID, EntryID: s.IDs.Next(), TopicID: f.TopicID, Question: f.Question,
		Answer: f.Answer, By: c.u.ID, At: s.Now().Unix()}
	if t := strings.TrimSpace(r.FormValue("new_topic")); t != "" || f.TopicID == 0 {
		if t == "" {
			t = "General"
		}
		add.NewTopic = &cmd.NewTopic{ID: s.IDs.Next(), Title: t}
	}
	_, err = s.Log.Apply(add)
	if s.commandFailed(w, r, c, err, "faq-edit", f) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/faq/e/%d", add.EntryID), http.StatusSeeOther)
}

// entryEditForm and entryEdit: a mod edits, moves, hides or locks an
// entry. ?use=suggestion starts from the rewrite waiting for a locked one.
func (s *Server) entryEditForm(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	f, err := s.entryForm(c, e)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.URL.Query().Get("use") == "suggestion" && e.Suggestion != "" {
		f.Answer = e.Suggestion
	}
	s.render(w, r, http.StatusOK, "faq-edit", c.page("Edit FAQ entry", f))
}

func (s *Server) entryEdit(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	f, err := s.entryForm(c, e)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f.Question, f.Answer = strings.TrimSpace(r.FormValue("question")), strings.TrimSpace(r.FormValue("answer"))
	f.TopicID, _ = strconv.ParseInt(r.FormValue("topic"), 10, 64)
	f.Locked, f.Hidden = r.FormValue("locked") == "on", r.FormValue("hidden") == "on"
	_, err = s.Log.Apply(&cmd.EditFAQEntry{GroupID: c.g.ID, EntryID: e.ID, TopicID: f.TopicID, Question: f.Question,
		Answer: f.Answer, Locked: f.Locked, Hidden: f.Hidden, By: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "faq-edit", f) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/faq/e/%d", e.ID), http.StatusSeeOther)
}

type historyData struct {
	Entry   *store.Entry
	History []historyView
}

type historyView struct {
	store.History
	By string // "automatic" or a mod's handle
}

// entryHistory lists every version of an entry, each with a rollback
// button (mods).
func (s *Server) entryHistory(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	hs, err := s.Store.EntryHistory(c.g.ID, e.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var ids []int64
	for _, h := range hs {
		ids = append(ids, h.ChangedBy)
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := historyData{Entry: e}
	for _, h := range hs {
		by := "automatic"
		if h.ChangedBy != 0 {
			by = names[h.ChangedBy]
		}
		d.History = append(d.History, historyView{History: h, By: by})
	}
	s.render(w, r, http.StatusOK, "faq-history", c.page("History: "+e.Question, d))
}

func (s *Server) entryRollback(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	h, _ := strconv.ParseInt(r.FormValue("version"), 10, 64)
	if _, err := s.Log.Apply(&cmd.RollbackFAQEntry{GroupID: c.g.ID, EntryID: e.ID, HistoryID: h, By: c.u.ID,
		At: s.Now().Unix()}); err != nil && !cmd.IsInput(err) {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/faq/e/%d", e.ID), http.StatusSeeOther)
}

// entryUnsource takes a thread out of an entry's sources (mods).
func (s *Server) entryUnsource(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	post := postRef(r.FormValue("post"))
	if _, err := s.Log.Apply(&cmd.RemoveFAQSource{GroupID: c.g.ID, EntryID: e.ID, PostID: post, By: c.u.ID, At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/faq/e/%d", e.ID), http.StatusSeeOther)
}

// entryComment adds a member's comment to an entry: new information for
// its next rewrite.
func (s *Server) entryComment(w http.ResponseWriter, r *http.Request) {
	c := s.faqCtx(w, r)
	if c == nil {
		return
	}
	if c.root {
		s.notFound(w, r)
		return
	}
	e := s.loadEntry(w, r, c)
	if e == nil {
		return
	}
	back := fmt.Sprintf("/faq/e/%d", e.ID)
	if !s.writer(w, r, c, back) {
		return
	}
	_, err := s.Log.Apply(&cmd.AddFAQComment{GroupID: c.g.ID, CommentID: s.IDs.Next(), EntryID: e.ID, UserID: c.u.ID,
		Body: r.FormValue("body"), At: s.Now().Unix()})
	if err != nil {
		d, derr := s.entryData(c, e)
		if derr != nil {
			s.serverError(w, r, derr)
			return
		}
		if s.commandFailed(w, r, c, err, "faq-entry", d) {
			return
		}
	}
	http.Redirect(w, r, back+"#comments", http.StatusSeeOther)
}

func (s *Server) entryCommentDelete(w http.ResponseWriter, r *http.Request) {
	c := s.faqCtx(w, r)
	if c == nil {
		return
	}
	id, ok := pathID(r)
	if !ok || c.u == nil {
		s.notFound(w, r)
		return
	}
	author, entry, err := s.Store.FAQCommentAuthor(c.g.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if entry == 0 || author != c.u.ID && !c.mod() {
		s.notFound(w, r)
		return
	}
	if _, err := s.Log.Apply(&cmd.RemoveFAQComment{GroupID: c.g.ID, CommentID: id, By: c.u.ID, ByMod: author != c.u.ID,
		At: s.Now().Unix()}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/faq/e/%d#comments", entry), http.StatusSeeOther)
}

// topicCreate adds a topic to the outline (mods).
func (s *Server) topicCreate(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	parent, _ := strconv.ParseInt(r.FormValue("parent"), 10, 64)
	_, err := s.Log.Apply(&cmd.CreateTopic{GroupID: c.g.ID, TopicID: s.IDs.Next(), ParentID: parent,
		Title: r.FormValue("title"), By: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't add that topic"}) {
		return
	}
	http.Redirect(w, r, "/faq", http.StatusSeeOther)
}

type topicData struct {
	Topic     *store.Topic
	Parent    *store.Topic
	Entries   []store.Entry
	Children  []store.Topic
	Posts     []postCard
	NextPage  int
	CanMod    bool
	AllTopics []store.Topic // for moving it (mods)
}

// topicPage is Browse by topic: a topic's FAQ entries and the threads on
// it, useful first, however far apart they were posted.
func (s *Server) topicPage(w http.ResponseWriter, r *http.Request) {
	c := s.faqCtx(w, r)
	if c == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	t, err := s.Store.Topic(c.g.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if t == nil {
		s.render(w, r, http.StatusNotFound, "notfound", c.page("Not found", nil))
		return
	}
	if t.ID != id {
		http.Redirect(w, r, fmt.Sprintf("/faq/t/%d", t.ID), http.StatusMovedPermanently) // merged into another
		return
	}
	d := topicData{Topic: t, CanMod: c.mod()}
	tree, err := s.Store.Topics(c.g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, top := range tree {
		if top.ID == t.ParentID {
			top := top
			d.Parent = &top
		}
		if top.ID == t.ID {
			d.Children = top.Children
		}
		if top.ID != t.ID && d.CanMod {
			d.AllTopics = append(d.AllTopics, top) // possible parents: main topics only
		}
	}
	entries, err := s.Store.Entries(c.g.ID, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	in := map[int64]bool{t.ID: true}
	for _, ch := range d.Children {
		in[ch.ID] = true
	}
	for _, e := range entries {
		if in[e.TopicID] {
			d.Entries = append(d.Entries, e)
		}
	}
	// Threads only for readers of the group itself (a public FAQ preview
	// of a private group shows the entries, not the threads).
	if !c.root && c.canRead(nil) {
		pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pg = max(pg, 0)
		posts, err := s.Store.TopicPosts(c.g.ID, t.ID, feedPageSize+1, pg*feedPageSize)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if len(posts) > feedPageSize {
			posts, d.NextPage = posts[:feedPageSize], pg+1
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
	s.render(w, r, http.StatusOK, "topic", c.page(t.Title, d))
}

// topicEdit renames or moves a topic (mods).
func (s *Server) topicEdit(w http.ResponseWriter, r *http.Request) {
	c := s.faqMod(w, r)
	if c == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	parent, _ := strconv.ParseInt(r.FormValue("parent"), 10, 64)
	sort, _ := strconv.ParseInt(r.FormValue("sort"), 10, 64)
	_, err := s.Log.Apply(&cmd.EditTopic{GroupID: c.g.ID, TopicID: id, Title: r.FormValue("title"), ParentID: parent,
		Sort: sort, By: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't change that topic"}) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/faq/t/%d", id), http.StatusSeeOther)
}

// postTopics: the author or a mod chooses a post's topics.
func (s *Server) postTopics(w http.ResponseWriter, r *http.Request) {
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
	r.ParseForm()
	var topics []int64
	for _, v := range r.PostForm["topic"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			topics = append(topics, id)
		}
	}
	_, err := s.Log.Apply(&cmd.SetPostTopics{GroupID: c.g.ID, PostID: p.ID, Topics: topics, By: c.u.ID, ByMod: !isAuthor, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't set topics"}) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/p/%d", p.ID), http.StatusSeeOther)
}

// reverseNudge undoes one display nudge (mods).
func (s *Server) reverseNudge(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	id, ok := pathID(r)
	if !ok || c.u == nil || !c.mod() {
		s.notFound(w, r)
		return
	}
	if _, err := s.Log.Apply(&cmd.ReverseNudge{GroupID: c.g.ID, NudgeID: id, By: c.u.ID, At: s.Now().Unix()}); err != nil && !cmd.IsInput(err) {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

// removeNote takes down a summary or combined note (mods).
func (s *Server) removeNote(w http.ResponseWriter, r *http.Request) {
	c := s.group(w, r)
	if c == nil {
		return
	}
	id, ok := pathID(r)
	if !ok || c.u == nil || !c.mod() {
		s.notFound(w, r)
		return
	}
	_, err := s.Log.Apply(&cmd.RemoveNote{GroupID: c.g.ID, NoteID: id, By: c.u.ID, At: s.Now().Unix()})
	if s.commandFailed(w, r, c, err, "message", message{Title: "Can't remove that"}) {
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

// backTo is where a small action returns: the page it came from, if that
// was on this same host, or the group's front page.
func backTo(r *http.Request) string {
	if ref := r.Referer(); ref != "" {
		if i := strings.Index(ref, "://"); i >= 0 {
			rest := ref[i+3:]
			if j := strings.IndexByte(rest, '/'); j >= 0 && strings.EqualFold(rest[:j], r.Host) {
				return rest[j:]
			}
		}
	}
	return "/"
}
