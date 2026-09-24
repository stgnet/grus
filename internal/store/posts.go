package store

import (
	"database/sql"
	"errors"
	"strings"
)

// Post is a post as pages show it.
type Post struct {
	ID             int64
	UserID         int64 // 0 for archive posts
	Anonymous      bool
	Title          string
	Body           string
	Status         string
	RemovedReason  string
	Pinned         bool
	Locked         bool
	Score          int
	CommentCount   int
	CreatedAt      int64
	EditedAt       int64
	LastActivityAt int64
	Origin         string // native | archive
	OriginURL      string // archive posts: the original's permalink, if known
	ThumbHash      string // first photo, for feed cards
	Digest         string // the thread's stored factual summary ("" until written)
	Version        int64  // edits to the post itself
	ThreadVersion  int64  // any change in the thread
	ContinuesID    int64  // set when this post is an Update under that post
}

// Comment is a comment as pages show it.
type Comment struct {
	ID            int64
	PostID        int64
	ParentID      int64
	UserID        int64
	Anonymous     bool
	Body          string
	Status        string
	RemovedReason string
	Score         int
	CreatedAt     int64
	EditedAt      int64
}

// Image is one photo on a post or comment.
type Image struct {
	ID        int64
	PostID    int64
	CommentID int64
	Hash      string
	Width     int
	Height    int
}

const postCols = `id, COALESCE(user_id, 0), is_anonymous, title, body, status, COALESCE(removed_reason, ''),
	pinned, locked, score, comment_count, created_at, COALESCE(edited_at, 0), last_activity_at, origin, COALESCE(origin_url, ''),
	COALESCE((SELECT blob_hash FROM images WHERE images.post_id = posts.id AND comment_id IS NULL
	          ORDER BY sort_order LIMIT 1), ''),
	COALESCE(digest, ''), version, thread_version, COALESCE(continues_post_id, 0)`

func scanPost(row interface{ Scan(...any) error }) (*Post, error) {
	var p Post
	err := row.Scan(&p.ID, &p.UserID, &p.Anonymous, &p.Title, &p.Body, &p.Status, &p.RemovedReason,
		&p.Pinned, &p.Locked, &p.Score, &p.CommentCount, &p.CreatedAt, &p.EditedAt, &p.LastActivityAt, &p.Origin, &p.OriginURL, &p.ThumbHash,
		&p.Digest, &p.Version, &p.ThreadVersion, &p.ContinuesID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Feed sorts.
const (
	SortActive = "active" // latest comment first, the default (like Facebook)
	SortNew    = "new"    // newest post first
)

// Feed lists a group's shown posts: pinned first, then by sort, one page at
// a time. Only visible and flagged posts appear in feeds; everything else is
// reachable only by its own link, subject to the read rule.
func (s *Store) Feed(groupID int64, sort string, limit, offset int) ([]Post, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	order := "last_activity_at DESC"
	if sort == SortNew {
		order = "created_at DESC"
	}
	rows, err := db.Query(`SELECT `+postCols+` FROM posts
		WHERE status IN ('visible', 'flagged') AND continues_post_id IS NULL
		ORDER BY pinned DESC, `+order+`, id DESC LIMIT ? OFFSET ?`, limit, offset)
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

// Post reads one post, whatever its status (the caller applies the read
// rule).
func (s *Store) Post(groupID, id int64) (*Post, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	return scanPost(db.QueryRow(`SELECT `+postCols+` FROM posts WHERE id = ?`, id))
}

const commentCols = `id, post_id, COALESCE(parent_id, 0), COALESCE(user_id, 0), is_anonymous, body, status,
	COALESCE(removed_reason, ''), score, created_at, COALESCE(edited_at, 0)`

func scanComment(row interface{ Scan(...any) error }) (*Comment, error) {
	var c Comment
	err := row.Scan(&c.ID, &c.PostID, &c.ParentID, &c.UserID, &c.Anonymous, &c.Body, &c.Status,
		&c.RemovedReason, &c.Score, &c.CreatedAt, &c.EditedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Comments lists every comment on a post, oldest first, whatever its status.
func (s *Store) Comments(groupID, postID int64) ([]Comment, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+commentCols+` FROM comments WHERE post_id = ? ORDER BY created_at, id`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Comment reads one comment.
func (s *Store) Comment(groupID, id int64) (*Comment, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	return scanComment(db.QueryRow(`SELECT `+commentCols+` FROM comments WHERE id = ?`, id))
}

// Images lists a post's photos (including those on its comments) in order.
func (s *Store) Images(groupID, postID int64) ([]Image, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, post_id, COALESCE(comment_id, 0), blob_hash, width, height
		FROM images WHERE post_id = ? ORDER BY sort_order, id`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var im Image
		if err := rows.Scan(&im.ID, &im.PostID, &im.CommentID, &im.Hash, &im.Width, &im.Height); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// ImageUses lists where a blob appears in a group, for the access check on
// /img: a photo may be seen by anyone who may read one of the items it's on.
func (s *Store) ImageUses(groupID int64, hash string) ([]Image, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, post_id, COALESCE(comment_id, 0), blob_hash, width, height
		FROM images WHERE blob_hash = ? LIMIT 20`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var im Image
		if err := rows.Scan(&im.ID, &im.PostID, &im.CommentID, &im.Hash, &im.Width, &im.Height); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// BlobHashes lists every blob any group references, for GC and for syncing
// photos to nodes that are missing them.
func (s *Store) BlobHashes() (map[string]bool, error) {
	out := map[string]bool{}
	ids, err := s.GroupFileIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		db, err := s.Group(id)
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(`SELECT DISTINCT blob_hash FROM images`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return nil, err
			}
			out[h] = true
		}
		rows.Close()
	}
	return out, nil
}

// Handles maps account ids to handles ("" for accounts without one, which
// includes deleted accounts after their purge).
func (s *Store) Handles(ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT id, COALESCE(handle, '') FROM users WHERE id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
	rows, err := s.Site().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// Mods lists a group's owners and mods (user ids), for the About page.
func (s *Store) Mods(groupID int64) ([]int64, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT user_id FROM memberships WHERE role IN ('owner', 'mod') AND status = 'active'
		ORDER BY role = 'mod', created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MemberCount counts a group's active members.
func (s *Store) MemberCount(groupID int64) (int, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE status = 'active'`).Scan(&n)
	return n, err
}
