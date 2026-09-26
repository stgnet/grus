package store

import (
	"database/sql"
	"errors"
	"slices"
)

// Reads for the group FAQ, topics, nudges and outside sources (M3).

// Topic is one heading in a group's FAQ outline.
type Topic struct {
	ID       int64
	ParentID int64
	Title    string
	Sort     int64
	Locked   bool
	Entries  int // active entries directly under it
	Children []Topic
}

// Topics lists a group's topics (not the ones merged away), as a tree:
// top-level topics with their children, each level in sort order.
func (s *Store) Topics(groupID int64) ([]Topic, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT t.id, t.parent_id, t.title, t.sort, t.locked,
		(SELECT COUNT(*) FROM faq_entries e WHERE e.topic_id = t.id AND e.status = 'active')
		FROM faq_topics t WHERE t.merged_into IS NULL ORDER BY t.sort, t.title COLLATE NOCASE, t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.ParentID, &t.Title, &t.Sort, &t.Locked, &t.Entries); err != nil {
			return nil, err
		}
		all = append(all, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var top []Topic
	for _, t := range all {
		if t.ParentID == 0 {
			top = append(top, t)
		}
	}
	for i := range top {
		for _, t := range all {
			if t.ParentID == top[i].ID {
				top[i].Children = append(top[i].Children, t)
			}
		}
	}
	return top, nil
}

// FlatTopics is the outline as one list in page order, parents before
// their children: for choosers and for the model.
func FlatTopics(tree []Topic) []Topic {
	var out []Topic
	for _, t := range tree {
		out = append(out, t)
		out = append(out, t.Children...)
	}
	return out
}

// Topic reads one topic, following merges to where it lives now.
func (s *Store) Topic(groupID, id int64) (*Topic, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	for range 10 {
		var t Topic
		var merged sql.NullInt64
		err := db.QueryRow(`SELECT id, parent_id, title, sort, locked, merged_into FROM faq_topics WHERE id = ?`, id).
			Scan(&t.ID, &t.ParentID, &t.Title, &t.Sort, &t.Locked, &merged)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !merged.Valid {
			return &t, nil
		}
		id = merged.Int64
	}
	return nil, nil
}

// Entry is one FAQ entry.
type Entry struct {
	ID         int64
	TopicID    int64
	Question   string
	Answer     string
	Status     string
	Locked     bool
	Stale      bool
	Version    int64
	Suggestion string
	CreatedAt  int64
	UpdatedAt  int64
	UpdatedBy  int64 // 0 = written by the system
	Sources    int   // how many threads it's based on
}

const entryCols = `id, topic_id, question, answer, status, locked, stale, version, COALESCE(suggestion, ''),
	created_at, updated_at, COALESCE(updated_by, 0),
	(SELECT COUNT(*) FROM faq_sources s JOIN posts p ON p.id = s.post_id
	 WHERE s.entry_id = faq_entries.id AND p.status IN ('visible', 'flagged'))`

func scanEntry(row interface{ Scan(...any) error }) (*Entry, error) {
	var e Entry
	err := row.Scan(&e.ID, &e.TopicID, &e.Question, &e.Answer, &e.Status, &e.Locked, &e.Stale, &e.Version,
		&e.Suggestion, &e.CreatedAt, &e.UpdatedAt, &e.UpdatedBy, &e.Sources)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) entries(groupID int64, where string, args ...any) ([]Entry, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+entryCols+` FROM faq_entries WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Entries lists a group's entries in page order (by topic, then the
// entry's own sort, then oldest first). withHidden includes hidden ones,
// for mods.
func (s *Store) Entries(groupID int64, withHidden bool) ([]Entry, error) {
	status := `status = 'active'`
	if withHidden {
		status = `1`
	}
	return s.entries(groupID, status+` ORDER BY topic_id, sort, id`)
}

// Entry reads one entry, whatever its status.
func (s *Store) Entry(groupID, id int64) (*Entry, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	return scanEntry(db.QueryRow(`SELECT `+entryCols+` FROM faq_entries WHERE id = ?`, id))
}

// EntriesForPost lists the active entries a post is a source of: shown on
// the post as "In the FAQ".
func (s *Store) EntriesForPost(groupID, postID int64) ([]Entry, error) {
	return s.entries(groupID, `status = 'active' AND id IN (SELECT entry_id FROM faq_sources WHERE post_id = ?) ORDER BY id`, postID)
}

// EntryCount counts a group's active entries.
func (s *Store) EntryCount(groupID int64) (int, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM faq_entries WHERE status = 'active'`).Scan(&n)
	return n, err
}

// EntryPosts lists the threads an entry is written from, newest first,
// whatever their status (callers apply the read rule).
func (s *Store) EntryPosts(groupID, entryID int64) ([]Post, error) {
	return s.posts(groupID, `id IN (SELECT post_id FROM faq_sources WHERE entry_id = ?) ORDER BY created_at DESC, id DESC`, entryID)
}

// posts runs a query for posts with a WHERE clause.
func (s *Store) posts(groupID int64, where string, args ...any) ([]Post, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+postCols+` FROM posts WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// History is one version of an entry.
type History struct {
	ID        int64
	Question  string
	Answer    string
	ChangedBy int64 // 0 = the system
	CreatedAt int64
}

// EntryHistory lists every version of an entry, newest first.
func (s *Store) EntryHistory(groupID, entryID int64) ([]History, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, question, answer, COALESCE(changed_by, 0), created_at FROM faq_history
		WHERE entry_id = ? ORDER BY created_at DESC, id DESC`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.Question, &h.Answer, &h.ChangedBy, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FAQComment is a member's comment on an entry.
type FAQComment struct {
	ID        int64
	UserID    int64
	Body      string
	CreatedAt int64
}

// EntryComments lists an entry's shown comments, oldest first.
func (s *Store) EntryComments(groupID, entryID int64) ([]FAQComment, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, user_id, body, created_at FROM faq_comments WHERE entry_id = ? AND status = 'visible'
		ORDER BY created_at, id`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FAQComment
	for rows.Next() {
		var c FAQComment
		if err := rows.Scan(&c.ID, &c.UserID, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FAQCommentAuthor says who wrote a comment on an entry (0 if none).
func (s *Store) FAQCommentAuthor(groupID, id int64) (userID, entryID int64, err error) {
	db, err := s.Group(groupID)
	if err != nil {
		return 0, 0, err
	}
	err = db.QueryRow(`SELECT user_id, entry_id FROM faq_comments WHERE id = ?`, id).Scan(&userID, &entryID)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

// PostTopics lists a post's topics, and whether a person chose them.
func (s *Store) PostTopics(groupID, postID int64) ([]Topic, bool, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, false, err
	}
	var manual bool
	db.QueryRow(`SELECT topics_manual FROM posts WHERE id = ?`, postID).Scan(&manual)
	rows, err := db.Query(`SELECT t.id, t.parent_id, t.title FROM post_topics pt JOIN faq_topics t ON t.id = pt.topic_id
		WHERE pt.post_id = ? AND t.merged_into IS NULL ORDER BY t.title`, postID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.ParentID, &t.Title); err != nil {
			return nil, false, err
		}
		out = append(out, t)
	}
	return out, manual, rows.Err()
}

// TopicPosts lists the threads on a topic for Browse by topic, useful
// first: threads in the FAQ, then the ones with the most discussion, then
// the most recently active. A subtopic's threads count for its parent.
func (s *Store) TopicPosts(groupID, topicID int64, limit, offset int) ([]Post, error) {
	return s.posts(groupID, `status IN ('visible', 'flagged') AND continues_post_id IS NULL AND id IN (
		SELECT post_id FROM post_topics WHERE topic_id = ?1 OR topic_id IN (SELECT id FROM faq_topics WHERE parent_id = ?1)
		UNION SELECT s.post_id FROM faq_sources s JOIN faq_entries e ON e.id = s.entry_id
		WHERE e.status = 'active' AND (e.topic_id = ?1 OR e.topic_id IN (SELECT id FROM faq_topics WHERE parent_id = ?1)))
		ORDER BY EXISTS (SELECT 1 FROM faq_sources WHERE post_id = posts.id) DESC, comment_count DESC, last_activity_at DESC
		LIMIT ?2 OFFSET ?3`, topicID, limit, offset)
}

// Cluster is the group of threads connected to a post through active
// links (shown posts only), oldest first, at most max of them: what a FAQ
// entry for that subject is written from.
func (s *Store) Cluster(groupID, postID int64, max int) ([]int64, error) {
	seen := map[int64]bool{postID: true}
	queue := []int64{postID}
	for i := 0; i < len(queue) && len(seen) < max; i++ {
		next, err := s.int64s(groupID, `SELECT CASE WHEN l.older_post_id = ?1 THEN l.newer_post_id ELSE l.older_post_id END AS o
			FROM post_links l JOIN posts p ON p.id = (CASE WHEN l.older_post_id = ?1 THEN l.newer_post_id ELSE l.older_post_id END)
			WHERE (l.older_post_id = ?1 OR l.newer_post_id = ?1) AND l.state = 'active' AND p.status = 'visible' ORDER BY o`, queue[i])
		if err != nil {
			return nil, err
		}
		for _, id := range next {
			if !seen[id] && len(seen) < max {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

// Nudge is one display-only change to how a thread is shown.
type Nudge struct {
	ID       int64
	Kind     string
	TargetID int64
	Value    int64
	Reason   string
	Reversed bool
}

// Nudges lists a thread's nudges, active and reversed.
func (s *Store) Nudges(groupID, postID int64) ([]Nudge, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, kind, target_id, value, reason, state = 'reversed' FROM nudges WHERE post_id = ? ORDER BY value, id`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Nudge
	for rows.Next() {
		var n Nudge
		if err := rows.Scan(&n.ID, &n.Kind, &n.TargetID, &n.Value, &n.Reason, &n.Reversed); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NoteSource is one thing a note is written from, or (Covers) a comment a
// summary folds under itself.
type NoteSource struct {
	GroupID   int64
	PostID    int64
	CommentID int64
	SourceID  int64
	Covers    bool
}

// NoteSources lists what a note is written from.
func (s *Store) NoteSources(groupID, noteID int64) ([]NoteSource, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT group_id, post_id, comment_id, source_id, covers FROM note_sources WHERE note_id = ?
		ORDER BY source_id != 0, post_id, comment_id, source_id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteSource
	for rows.Next() {
		var n NoteSource
		if err := rows.Scan(&n.GroupID, &n.PostID, &n.CommentID, &n.SourceID, &n.Covers); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
