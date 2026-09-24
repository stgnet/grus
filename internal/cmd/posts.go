package cmd

import (
	"database/sql"
	"errors"
	"fmt"
)

// Posts, comments, and the soft-delete / restore pair (plan section 2,
// "Posting and discussion", and section 6, "Deleting: hidden now, purged
// later").
//
// Who may do what (the author edits their own post; mods remove anything)
// is checked by the handlers before submitting. Apply checks what keeps
// the data sound: the post exists, is open for comments, and so on.

// Retention before the daily Purge removes something for good.
const (
	day                 = 24 * 60 * 60
	AuthorDeleteKeep    = 30 * day // author deleted it: mods can still restore
	ModRemoveKeep       = 90 * day // mods removed it: kept for appeals
	RevisionKeep        = 90 * day // earlier versions after an edit
	MaxImagesPerPost    = 10
	MaxTitleLen         = 300
	MaxBodyLen          = 40000
	MaxCommentLen       = 10000
	ArchiveAuthorHandle = "Facebook member" // imported posts have no account
)

var (
	ErrNotMember = errors.New("join the group first")
	ErrLocked    = errors.New("this post is locked")
	ErrGone      = errors.New("that post or comment isn't available")
)

// Image places an already-stored blob on a post or comment.
type Image struct {
	ID     int64
	Hash   string
	Width  int
	Height int
	Bytes  int
}

// requireMember fails unless userID is an active member. (A ban ends
// membership; when a temporary ban runs out, they can join again.)
func requireMember(tx *sql.Tx, userID int64) error {
	var status string
	err := tx.QueryRow(`SELECT status FROM memberships WHERE user_id = ?`, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || status != "active" {
		return ErrNotMember
	}
	return err
}

// ftsPut indexes (or re-indexes) one item for search. rowid is the item's id.
func ftsPut(tx *sql.Tx, kind string, id, postID int64, title, body string) error {
	if _, err := tx.Exec(`DELETE FROM search_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO search_fts (rowid, kind, ref_id, post_id, title, body) VALUES (?, ?, ?, ?, ?, ?)`,
		id, kind, id, postID, title, body)
	return err
}

func ftsDel(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`DELETE FROM search_fts WHERE rowid = ?`, id)
	return err
}

func insertImages(tx *sql.Tx, postID int64, commentID *int64, userID any, imgs []Image, at int64) error {
	for i, im := range imgs {
		if _, err := tx.Exec(`INSERT INTO images (id, post_id, comment_id, user_id, blob_hash, width, height, bytes, sort_order, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			im.ID, postID, commentID, userID, im.Hash, im.Width, im.Height, im.Bytes, i, at); err != nil {
			return err
		}
	}
	return nil
}

// CreatePost adds a post with up to 10 photos.
type CreatePost struct {
	GroupID   int64
	PostID    int64
	UserID    int64
	Title     string
	Body      string
	Anonymous bool
	Images    []Image
	At        int64
}

func (c *CreatePost) Apply(a *Applier) (any, error) {
	if c.Title == "" || len(c.Title) > MaxTitleLen || len(c.Body) > MaxBodyLen || len(c.Images) > MaxImagesPerPost {
		return nil, Invalid("a post needs a title (up to %d characters), text up to %d, and at most %d photos",
			MaxTitleLen, MaxBodyLen, MaxImagesPerPost)
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := requireMember(tx, c.UserID); err != nil {
			return err
		}
		status, err := firstPostStatus(tx, c.UserID)
		if err != nil {
			return err
		}
		if err := anonymousAllowed(tx, c.Anonymous); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO posts (id, user_id, is_anonymous, title, body, status, created_at, last_activity_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.PostID, c.UserID, c.Anonymous, c.Title, c.Body, status, c.At, c.At)
		if err != nil {
			return err
		}
		if err := insertImages(tx, c.PostID, nil, c.UserID, c.Images, c.At); err != nil {
			return err
		}
		// Authors follow their own posts, so they hear about replies.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO follows (user_id, post_id, created_at) VALUES (?, ?, ?)`,
			c.UserID, c.PostID, c.At); err != nil {
			return err
		}
		// Check it now (links to older posts; moderation from M5), and
		// write its digest once the first replies settle.
		if err := schedule(tx, JobCheck, c.PostID, 1, c.At, c.At); err != nil {
			return err
		}
		if err := schedule(tx, JobDigest, c.PostID, 1, c.At+QuietPeriod, c.At); err != nil {
			return err
		}
		if status != "visible" {
			return nil // not searchable until it's shown
		}
		return ftsPut(tx, "post", c.PostID, c.PostID, c.Title, c.Body)
	})
}

// firstPostStatus is "held" for a newcomer's first post in a group that
// holds them (plan section 6, "hold new members' first post"): someone who
// has nothing shown in the group yet, and isn't one of its mods. It's
// decided here, from the group's own data, so every node agrees.
func firstPostStatus(tx *sql.Tx, userID int64) (string, error) {
	var hold bool
	if err := tx.QueryRow(`SELECT hold_first_post FROM settings WHERE id = 1`).Scan(&hold); err != nil || !hold {
		return "visible", err
	}
	var role string
	var shown int
	tx.QueryRow(`SELECT role FROM memberships WHERE user_id = ?`, userID).Scan(&role)
	tx.QueryRow(`SELECT (SELECT COUNT(*) FROM posts WHERE user_id = ?1 AND status IN ('visible', 'flagged'))
		+ (SELECT COUNT(*) FROM comments WHERE user_id = ?1 AND status IN ('visible', 'flagged'))`, userID).Scan(&shown)
	if shown > 0 || role == "owner" || role == "mod" {
		return "visible", nil
	}
	return "held", nil
}

// EditPost changes a post's title and text, keeping the old version.
type EditPost struct {
	GroupID  int64
	PostID   int64
	EditorID int64
	Title    string
	Body     string
	At       int64
}

func (c *EditPost) Apply(a *Applier) (any, error) {
	if c.Title == "" || len(c.Title) > MaxTitleLen || len(c.Body) > MaxBodyLen {
		return nil, Invalid("a post needs a title (up to %d characters) and text up to %d", MaxTitleLen, MaxBodyLen)
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var title, body, status string
		err := tx.QueryRow(`SELECT title, body, status FROM posts WHERE id = ?`, c.PostID).Scan(&title, &body, &status)
		if err != nil {
			return notFoundGone(err)
		}
		if status == "deleted" || status == "removed" {
			return ErrGone
		}
		if err := saveRevision(tx, "post", c.PostID, &title, body, c.EditorID, c.At); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE posts SET title = ?, body = ?, edited_at = ? WHERE id = ?`,
			c.Title, c.Body, c.At, c.PostID); err != nil {
			return err
		}
		if err := postEdited(tx, c.GroupID, c.PostID, c.At); err != nil {
			return err
		}
		if status == "visible" || status == "flagged" {
			return ftsPut(tx, "post", c.PostID, c.PostID, c.Title, c.Body)
		}
		return nil
	})
}

// saveRevision keeps the version being replaced, numbered 1, 2, 3...
func saveRevision(tx *sql.Tx, kind string, id int64, title *string, body string, editor, at int64) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM revisions WHERE kind = ? AND ref_id = ?`, kind, id).Scan(&n); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO revisions (kind, ref_id, version, title, body, edited_by, edited_at, purge_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, kind, id, n+1, title, body, editor, at, at+RevisionKeep)
	return err
}

// CreateComment adds a comment, or a reply to one. Replies are one level
// deep (Facebook-style): a reply to a reply attaches to the top comment.
type CreateComment struct {
	GroupID   int64
	CommentID int64
	PostID    int64
	ParentID  int64 // 0 = a top-level comment
	UserID    int64
	Body      string
	Anonymous bool
	Image     *Image
	At        int64
}

func (c *CreateComment) Apply(a *Applier) (any, error) {
	if c.Body == "" && c.Image == nil || len(c.Body) > MaxCommentLen {
		return nil, Invalid("a comment needs text (up to %d characters) or a photo", MaxCommentLen)
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := requireMember(tx, c.UserID); err != nil {
			return err
		}
		if err := anonymousAllowed(tx, c.Anonymous); err != nil {
			return err
		}
		var status string
		var locked bool
		if err := tx.QueryRow(`SELECT status, locked FROM posts WHERE id = ?`, c.PostID).Scan(&status, &locked); err != nil {
			return notFoundGone(err)
		}
		if status != "visible" && status != "flagged" {
			return ErrGone
		}
		if locked {
			return ErrLocked
		}
		var parent any // NULL for a top-level comment
		if c.ParentID != 0 {
			var pid sql.NullInt64
			var ppost int64
			if err := tx.QueryRow(`SELECT post_id, parent_id FROM comments WHERE id = ?`, c.ParentID).Scan(&ppost, &pid); err != nil {
				return notFoundGone(err)
			}
			if ppost != c.PostID {
				return ErrGone
			}
			parent = c.ParentID
			if pid.Valid {
				parent = pid.Int64
			}
		}
		if _, err := tx.Exec(`INSERT INTO comments (id, post_id, parent_id, user_id, is_anonymous, body, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, c.CommentID, c.PostID, parent, c.UserID, c.Anonymous, c.Body, c.At); err != nil {
			return err
		}
		if c.Image != nil {
			cid := c.CommentID
			if err := insertImages(tx, c.PostID, &cid, c.UserID, []Image{*c.Image}, c.At); err != nil {
				return err
			}
		}
		// Denormalized counters, updated here so feeds never count or join.
		if _, err := tx.Exec(`UPDATE posts SET comment_count = comment_count + 1, last_activity_at = ? WHERE id = ?`,
			c.At, c.PostID); err != nil {
			return err
		}
		if err := threadChanged(tx, c.GroupID, c.PostID, c.At); err != nil {
			return err
		}
		// The moderation check (M5), a few seconds after it's saved.
		if err := schedule(tx, JobCheckComment, c.CommentID, 1, c.At, c.At); err != nil {
			return err
		}
		if err := commentNotices(tx, c.PostID, c.CommentID, c.ParentID, c.UserID, c.At); err != nil {
			return err
		}
		return ftsPut(tx, "comment", c.CommentID, c.PostID, "", c.Body)
	})
}

// EditComment changes a comment's text, keeping the old version.
type EditComment struct {
	GroupID   int64
	CommentID int64
	EditorID  int64
	Body      string
	At        int64
}

func (c *EditComment) Apply(a *Applier) (any, error) {
	if c.Body == "" || len(c.Body) > MaxCommentLen {
		return nil, Invalid("a comment needs text, up to %d characters", MaxCommentLen)
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var body, status string
		var postID int64
		err := tx.QueryRow(`SELECT body, status, post_id FROM comments WHERE id = ?`, c.CommentID).Scan(&body, &status, &postID)
		if err != nil {
			return notFoundGone(err)
		}
		if status == "deleted" || status == "removed" {
			return ErrGone
		}
		if err := saveRevision(tx, "comment", c.CommentID, nil, body, c.EditorID, c.At); err != nil {
			return err
		}
		var v int64
		if err := tx.QueryRow(`UPDATE comments SET body = ?, edited_at = ?, version = version + 1 WHERE id = ? RETURNING version`,
			c.Body, c.At, c.CommentID).Scan(&v); err != nil {
			return err
		}
		if err := schedule(tx, JobCheckComment, c.CommentID, v, c.At, c.At); err != nil {
			return err
		}
		if err := threadChanged(tx, c.GroupID, postID, c.At); err != nil {
			return err
		}
		if status == "visible" || status == "flagged" {
			return ftsPut(tx, "comment", c.CommentID, postID, "", c.Body)
		}
		return nil
	})
}

// SoftDelete hides a post or comment now and schedules it for purging:
// "deleted" when its author deletes it (30 days), "removed" when a mod does
// (90 days, for appeals). It leaves the search index in the same
// transaction, so it disappears from search at once.
type SoftDelete struct {
	GroupID int64
	Kind    string // post | comment
	ID      int64
	By      int64
	ByMod   bool
	Reason  string
	At      int64
}

func (c *SoftDelete) Apply(a *Applier) (any, error) {
	status, keep := "deleted", int64(AuthorDeleteKeep)
	if c.ByMod {
		status, keep = "removed", ModRemoveKeep
	}
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		table, err := itemTable(c.Kind)
		if err != nil {
			return err
		}
		var was string
		if err := tx.QueryRow(`SELECT status FROM `+table+` WHERE id = ?`, c.ID).Scan(&was); err != nil {
			return notFoundGone(err)
		}
		if was == "deleted" || was == "removed" {
			return nil // already gone; deleting twice is harmless
		}
		if _, err := tx.Exec(`UPDATE `+table+` SET status = ?, removed_reason = ?, deleted_at = ?, deleted_by = ?, purge_after = ?
			WHERE id = ?`, status, nullIfEmpty(c.Reason), c.At, c.By, c.At+keep, c.ID); err != nil {
			return err
		}
		if err := ftsDel(tx, c.ID); err != nil {
			return err
		}
		if err := threadChanged(tx, c.GroupID, threadOf(tx, c.Kind, c.ID), c.At); err != nil {
			return err
		}
		if c.Kind == "comment" && (was == "visible" || was == "flagged") {
			if _, err := tx.Exec(`UPDATE posts SET comment_count = comment_count - 1
				WHERE id = (SELECT post_id FROM comments WHERE id = ?)`, c.ID); err != nil {
				return err
			}
		}
		if c.Kind == "post" && (was == "visible" || was == "flagged") {
			// Notes in sister groups written from this post stop showing too.
			refs, err := sisterRefs(tx, c.ID)
			if err != nil {
				return err
			}
			if err := sendSisterNotes(tx, c.GroupID, c.ID, refs, false, c.At); err != nil {
				return err
			}
		}
		if c.ByMod {
			// A mod removing something is an example for the AI check of
			// what this group doesn't accept, and settles any reports.
			it, err := loadItem(tx, c.Kind, c.ID)
			if err != nil {
				return err
			}
			if err := resolveReports(tx, it, c.At); err != nil {
				return err
			}
			if err := modExample(tx, it, "hide", c.At); err != nil {
				return err
			}
			if err := authorNotice(tx, it, NoteRemoved, c.By, c.At); err != nil {
				return err
			}
			return modLog(tx, c.By, "remove", c.Kind, c.ID, c.Reason, c.At)
		}
		return nil
	})
	return nil, err
}

// Restore undoes a SoftDelete (mods only), any time before the purge.
type Restore struct {
	GroupID int64
	Kind    string
	ID      int64
	By      int64
	At      int64
}

func (c *Restore) Apply(a *Applier) (any, error) {
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		table, err := itemTable(c.Kind)
		if err != nil {
			return err
		}
		var was, body string
		var title sql.NullString
		var postID int64
		q := `SELECT status, title, body, id FROM posts WHERE id = ?`
		if c.Kind == "comment" {
			q = `SELECT status, NULL, body, post_id FROM comments WHERE id = ?`
		}
		if err := tx.QueryRow(q, c.ID).Scan(&was, &title, &body, &postID); err != nil {
			return notFoundGone(err)
		}
		if was == "visible" {
			return nil
		}
		if _, err := tx.Exec(`UPDATE `+table+` SET status = 'visible', removed_reason = NULL, deleted_at = NULL,
			deleted_by = NULL, purge_after = NULL WHERE id = ?`, c.ID); err != nil {
			return err
		}
		if err := ftsPut(tx, c.Kind, c.ID, postID, title.String, body); err != nil {
			return err
		}
		if err := threadChanged(tx, c.GroupID, postID, c.At); err != nil {
			return err
		}
		if c.Kind == "comment" && was != "flagged" {
			if _, err := tx.Exec(`UPDATE posts SET comment_count = comment_count + 1 WHERE id = ?`, postID); err != nil {
				return err
			}
		}
		if c.Kind == "post" {
			refs, err := sisterRefs(tx, c.ID)
			if err != nil {
				return err
			}
			if err := sendSisterNotes(tx, c.GroupID, c.ID, refs, true, c.At); err != nil {
				return err
			}
		}
		return modLog(tx, c.By, "restore", c.Kind, c.ID, "", c.At)
	})
	return nil, err
}

// threadOf is the post a post or comment belongs to.
func threadOf(tx *sql.Tx, kind string, id int64) int64 {
	if kind == "post" {
		return id
	}
	var postID int64
	tx.QueryRow(`SELECT post_id FROM comments WHERE id = ?`, id).Scan(&postID)
	return postID
}

func itemTable(kind string) (string, error) {
	switch kind {
	case "post":
		return "posts", nil
	case "comment":
		return "comments", nil
	}
	return "", fmt.Errorf("unknown kind %q", kind)
}

// modLog records a mod action. actor 0 = automatic (AI or a member vote).
func modLog(tx *sql.Tx, actor int64, action, targetType string, targetID int64, reason string, at int64) error {
	var who any
	if actor != 0 {
		who = actor
	}
	_, err := tx.Exec(`INSERT INTO mod_log (actor_id, action, target_type, target_id, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, who, action, targetType, targetID, reason, at)
	return err
}

func notFoundGone(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrGone
	}
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
