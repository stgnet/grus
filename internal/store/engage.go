package store

import (
	"strconv"
	"strings"
)

// Reads for M6: the Top sort, helpful votes, follows, notifications, and
// what the email pass and the daily digest need.

// Top-sort windows, in seconds; "all" is no window.
var TopWindows = map[string]int64{
	"week":  7 * 86400,
	"month": 30 * 86400,
	"year":  365 * 86400,
	"all":   0,
}

// SortTop is the Top feed: most helpful first, within a window.
const SortTop = "top"

// FeedTop lists a group's shown posts made since `since` (0 = ever), most
// helpful first. Pinned posts aren't lifted here: Top is a ranking, and a
// pinned rules post would sit on top of every week.
func (s *Store) FeedTop(groupID, since int64, limit, offset int) ([]Post, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+postCols+` FROM posts
		WHERE status IN ('visible', 'flagged') AND continues_post_id IS NULL AND created_at >= ?
		ORDER BY score DESC, comment_count DESC, id DESC LIMIT ? OFFSET ?`, since, limit, offset)
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

// Following reports whether userID follows postID.
func (s *Store) Following(groupID, postID, userID int64) (bool, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return false, err
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM follows WHERE user_id = ? AND post_id = ?`, userID, postID).Scan(&n)
	return n > 0, err
}

// MyHelpful lists which items in a thread (the post, its updates, and all
// their comments) userID marked helpful, keyed "post:<id>" / "comment:<id>".
func (s *Store) MyHelpful(groupID, postID, userID int64) (map[string]bool, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT kind, item_id FROM votes WHERE user_id = ?1 AND (
		  (kind = 'post' AND item_id IN (SELECT id FROM posts WHERE id = ?2 OR continues_post_id = ?2))
		  OR (kind = 'comment' AND item_id IN (SELECT id FROM comments WHERE post_id IN
		       (SELECT id FROM posts WHERE id = ?2 OR continues_post_id = ?2))))`, userID, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind string
		var id int64
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, err
		}
		out[kind+":"+strconv.FormatInt(id, 10)] = true
	}
	return out, rows.Err()
}

// Notification is one row of the bell, with what it takes to word it.
type Notification struct {
	ID         int64
	GroupID    int64
	UserID     int64
	Kind       string
	PostID     int64
	RefID      int64
	ActorID    int64
	ReadAt     int64
	CreatedAt  int64
	PostTitle  string
	PostAuthor int64 // "your post" or not
	Anonymous  bool  // the comment it's about was posted anonymously
	RefKind    string
}

const noteCols = `n.id, n.user_id, n.kind, n.post_id, n.ref_id, n.actor_id, COALESCE(n.read_at, 0), n.created_at,
	COALESCE(p.title, ''), COALESCE(p.user_id, 0), COALESCE(c.is_anonymous, 0)`

// The comment join only applies where ref_id is a comment: for "linked",
// ref_id is the newer post.
const noteFrom = ` FROM notifications n LEFT JOIN posts p ON p.id = n.post_id
	LEFT JOIN comments c ON c.id = n.ref_id AND n.kind != 'linked'`

func (s *Store) scanNotes(groupID int64, q string, args ...any) ([]Notification, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		n := Notification{GroupID: groupID}
		if err := rows.Scan(&n.ID, &n.UserID, &n.Kind, &n.PostID, &n.RefID, &n.ActorID, &n.ReadAt, &n.CreatedAt,
			&n.PostTitle, &n.PostAuthor, &n.Anonymous); err != nil {
			return nil, err
		}
		n.RefKind = "post"
		if n.Kind != "linked" && n.RefID != 0 {
			n.RefKind = "comment"
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Notifications lists someone's newest notifications in a group.
func (s *Store) Notifications(groupID, userID int64, limit int) ([]Notification, error) {
	return s.scanNotes(groupID, `SELECT `+noteCols+noteFrom+` WHERE n.user_id = ? ORDER BY n.id DESC LIMIT ?`, userID, limit)
}

// UnreadCount counts someone's unread notifications in a group.
func (s *Store) UnreadCount(groupID, userID int64) (int, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL`, userID).Scan(&n)
	return n, err
}

// PendingEmail lists the notifications in a group that are unread, not
// yet emailed, made at or before `before` (so a burst of replies waits a
// little and goes as one email), and for one of userIDs (the people who
// want email), oldest first.
func (s *Store) PendingEmail(groupID, before int64, userIDs []int64) ([]Notification, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	args := []any{before}
	for _, id := range userIDs {
		args = append(args, id)
	}
	return s.scanNotes(groupID, `SELECT `+noteCols+noteFrom+`
		WHERE n.read_at IS NULL AND n.emailed_at IS NULL AND n.created_at <= ?
		  AND n.user_id IN (?`+strings.Repeat(",?", len(userIDs)-1)+`)
		ORDER BY n.id LIMIT 5000`, args...)
}

// EmailUsers lists the accounts that want notifications by email, or a
// digest: everyone the email pass may write to.
func (s *Store) EmailUsers() (map[int64]*User, error) {
	rows, err := s.Site().Query(`SELECT ` + userCols + ` FROM users
		WHERE (notify_email = 1 OR digest != 'off') AND email IS NOT NULL AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

// NewTopPosts lists a group's shown posts made since `since`, best first,
// for the digest.
func (s *Store) NewTopPosts(groupID, since int64, limit int) ([]Post, error) {
	return s.FeedTop(groupID, since, limit, 0)
}
