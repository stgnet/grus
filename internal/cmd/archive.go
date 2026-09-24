package cmd

import (
	"database/sql"
	"errors"
)

// ImportPost loads one thread from an existing knowledge base (Scott's
// Travato archive) as an authorless archive post. It's an upsert keyed on
// the original's reference (OriginRef), so running an import again updates
// threads in place instead of duplicating them.
//
// Archive posts have no account: user_id is NULL and the author shows as
// ArchiveAuthorHandle. Nobody's name comes across. They're imported locked:
// an archive thread is a record of what was said, not a place to reply
// (a member who has more to add starts a new post, which links back).
type ImportPost struct {
	GroupID      int64
	PostID       int64 // used only if the thread is new
	OriginRef    string
	Permalink    string // the original's web address, if the knowledge base has it
	Title        string
	Body         string
	CreatedAt    int64
	LastActivity int64
	Images       []Image
	Comments     []ArchiveComment
	At           int64 // when the import ran, for scheduling its AI work
}

// ArchiveComment is one comment in an imported thread. ParentRef names the
// comment it replies to ("" for a top-level comment).
type ArchiveComment struct {
	ID        int64 // used only if the comment is new
	OriginRef string
	ParentRef string
	Body      string
	CreatedAt int64
	Image     *Image
}

func (c *ImportPost) Apply(a *Applier) (any, error) {
	if c.OriginRef == "" || c.Title == "" {
		return nil, errors.New("an archive post needs a reference and a title")
	}
	var postID int64
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		var oldTitle, oldBody, status string
		isNew, postChanged, threadChanges := false, false, false
		err := tx.QueryRow(`SELECT id, title, body, status FROM posts WHERE origin_ref = ?`, c.OriginRef).Scan(&postID, &oldTitle, &oldBody, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			isNew = true
			postID = c.PostID
			if _, err := tx.Exec(`INSERT INTO posts (id, user_id, title, body, created_at, last_activity_at, locked, origin, origin_ref, origin_url)
				VALUES (?, NULL, ?, ?, ?, ?, 1, 'archive', ?, ?)`, postID, c.Title, c.Body, c.CreatedAt, c.LastActivity,
				c.OriginRef, nullIfEmpty(c.Permalink)); err != nil {
				return err
			}
		case err != nil:
			return err
		case status != "visible" && status != "flagged":
			// Taken down (a removal request, or a mod): a re-import must
			// not bring it back.
			return nil
		default:
			postChanged = oldTitle != c.Title || oldBody != c.Body
			if _, err := tx.Exec(`UPDATE posts SET title = ?, body = ?, last_activity_at = ?, origin_url = ? WHERE id = ?`,
				c.Title, c.Body, c.LastActivity, nullIfEmpty(c.Permalink), postID); err != nil {
				return err
			}
		}
		// Photos are replaced wholesale; the blobs themselves are shared and
		// content-addressed, so nothing is re-uploaded.
		if _, err := tx.Exec(`DELETE FROM images WHERE post_id = ?`, postID); err != nil {
			return err
		}
		if err := insertImages(tx, postID, nil, nil, c.Images, c.CreatedAt); err != nil {
			return err
		}
		if err := ftsPut(tx, "post", postID, postID, c.Title, c.Body); err != nil {
			return err
		}

		ids := map[string]int64{} // origin ref -> comment id, for parents
		for _, cm := range c.Comments {
			var id int64
			var oldBody, cstatus string
			err := tx.QueryRow(`SELECT id, body, status FROM comments WHERE origin_ref = ?`, cm.OriginRef).Scan(&id, &oldBody, &cstatus)
			var parent any
			if pid, ok := ids[cm.ParentRef]; ok {
				parent = pid
			}
			switch {
			case errors.Is(err, sql.ErrNoRows):
				id = cm.ID
				threadChanges = true
				if _, err := tx.Exec(`INSERT INTO comments (id, post_id, parent_id, user_id, body, created_at, origin_ref)
					VALUES (?, ?, ?, NULL, ?, ?, ?)`, id, postID, parent, cm.Body, cm.CreatedAt, cm.OriginRef); err != nil {
					return err
				}
			case err != nil:
				return err
			case cstatus != "visible" && cstatus != "flagged":
				ids[cm.OriginRef] = id
				continue // taken down; leave it that way
			default:
				threadChanges = threadChanges || oldBody != cm.Body
				if _, err := tx.Exec(`UPDATE comments SET body = ?, parent_id = ? WHERE id = ?`, cm.Body, parent, id); err != nil {
					return err
				}
			}
			ids[cm.OriginRef] = id
			if cm.Image != nil {
				cid := id
				if err := insertImages(tx, postID, &cid, nil, []Image{*cm.Image}, cm.CreatedAt); err != nil {
					return err
				}
			}
			if err := ftsPut(tx, "comment", id, postID, "", cm.Body); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE posts SET comment_count =
			(SELECT COUNT(*) FROM comments WHERE post_id = ?1 AND status IN ('visible', 'flagged')) WHERE id = ?1`, postID); err != nil {
			return err
		}
		// Archive threads go through the same pipeline as new posts (checked
		// for links, digested), and a re-import only queues work for what
		// actually changed.
		switch {
		case isNew:
			if err := schedule(tx, JobCheck, postID, 1, c.At, c.At); err != nil {
				return err
			}
			return schedule(tx, JobDigest, postID, 1, c.At, c.At)
		case postChanged:
			return postEdited(tx, c.GroupID, postID, c.At)
		case threadChanges:
			return threadChanged(tx, c.GroupID, postID, c.At)
		}
		return nil
	})
	return postID, err
}
