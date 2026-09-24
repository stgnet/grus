package cmd

import (
	"database/sql"
)

// Engagement (plan section 2): "helpful" votes for the Top sort, following
// posts, and the notifications that come from both. Plan section 5 lists
// what notifies: replies to your posts and comments, new comments on posts
// you follow, newer posts linked to them, join approvals, and mod actions
// on your content.
//
// Notifications are rows in the group's own file, written by the same
// command as the thing they're about, so a reply and its notification can't
// come apart. Email is sent later, by the leader only (internal/web/email.go),
// and recorded with MarkEmailed so it goes once.

// Notification kinds.
const (
	NoteReply    = "reply"    // a reply to your comment
	NoteComment  = "comment"  // a new comment on a post you follow (your own included)
	NoteLinked   = "linked"   // a newer post was linked to one you follow
	NoteJoined   = "joined"   // your request to join was approved
	NoteHidden   = "hidden"   // your post or comment is hidden until a mod looks
	NoteRemoved  = "removed"  // a mod removed your post or comment
	NoteApproved = "approved" // your held first post is up
)

// Digest choices for SetNotifyPrefs.
const (
	DigestOff   = "off"
	DigestDaily = "daily"
)

// notify adds one notification. Nobody is told about their own action,
// and only active members are told anything: someone who left or was
// banned has no business hearing about the group.
func notify(tx *sql.Tx, userID int64, kind string, postID, refID, actorID, at int64) error {
	if userID == 0 || userID == actorID {
		return nil
	}
	var status string
	if tx.QueryRow(`SELECT status FROM memberships WHERE user_id = ?`, userID).Scan(&status); status != "active" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO notifications (user_id, kind, post_id, ref_id, actor_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, kind, postID, refID, actorID, at)
	return err
}

// notifyFollowers tells everyone following postID, except those in skip
// (already told something more specific, or the one who caused it).
func notifyFollowers(tx *sql.Tx, postID int64, kind string, refID, actorID int64, skip map[int64]bool, at int64) error {
	followers, err := queryIDs(tx, `SELECT user_id FROM follows WHERE post_id = ? ORDER BY user_id`, postID)
	if err != nil {
		return err
	}
	for _, u := range followers {
		if skip[u] {
			continue
		}
		if err := notify(tx, u, kind, postID, refID, actorID, at); err != nil {
			return err
		}
	}
	return nil
}

// commentNotices is what a new comment sets off: a "reply" to the author
// of the comment it answers, and a "comment" to the post's followers.
// Nobody gets both. replyTo is the comment the person pressed Reply on
// (0 for a top-level comment).
func commentNotices(tx *sql.Tx, postID, commentID, replyTo, actorID, at int64) error {
	skip := map[int64]bool{actorID: true}
	if replyTo != 0 {
		var author sql.NullInt64
		tx.QueryRow(`SELECT user_id FROM comments WHERE id = ?`, replyTo).Scan(&author)
		if author.Valid {
			if err := notify(tx, author.Int64, NoteReply, postID, commentID, actorID, at); err != nil {
				return err
			}
			skip[author.Int64] = true
		}
	}
	return notifyFollowers(tx, postID, NoteComment, commentID, actorID, skip, at)
}

// authorNotice tells an item's author what happened to it: hidden or
// removed (unless the group chose quiet hiding, notify_hidden off), or let
// through. For a comment, refID is the comment.
func authorNotice(tx *sql.Tx, it *item, kind string, by, at int64) error {
	if kind == NoteHidden || kind == NoteRemoved {
		var tell bool
		tx.QueryRow(`SELECT notify_hidden FROM settings WHERE id = 1`).Scan(&tell)
		if !tell {
			return nil
		}
	}
	var ref int64
	if it.kind == "comment" {
		ref = it.id
	}
	return notify(tx, it.author, kind, it.postID, ref, by, at)
}

// Helpful is a member's "helpful" vote on a post or comment (or taking it
// back). It only sorts: Top orders posts by it, and nothing is hidden or
// shown because of it.
type Helpful struct {
	GroupID int64
	Kind    string // post | comment
	ID      int64
	UserID  int64
	On      bool
	At      int64
}

func (c *Helpful) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := requireMember(tx, c.UserID); err != nil {
			return err
		}
		it, err := loadItem(tx, c.Kind, c.ID)
		if err != nil {
			return err
		}
		if !shown(it.status) {
			return ErrGone
		}
		if it.author == c.UserID {
			return Invalid("that's your own")
		}
		if c.On {
			_, err = tx.Exec(`INSERT OR IGNORE INTO votes (kind, item_id, user_id, created_at) VALUES (?, ?, ?, ?)`,
				c.Kind, c.ID, c.UserID, c.At)
		} else {
			_, err = tx.Exec(`DELETE FROM votes WHERE kind = ? AND item_id = ? AND user_id = ?`, c.Kind, c.ID, c.UserID)
		}
		if err != nil {
			return err
		}
		// Recounted rather than incremented, so the stored score can't
		// drift from the votes behind it.
		_, err = tx.Exec(`UPDATE `+it.table()+` SET score = (SELECT COUNT(*) FROM votes WHERE kind = ? AND item_id = ?) WHERE id = ?`,
			c.Kind, c.ID, c.ID)
		return err
	})
}

// Follow starts or stops following a post.
type Follow struct {
	GroupID int64
	PostID  int64
	UserID  int64
	On      bool
	At      int64
}

func (c *Follow) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := requireMember(tx, c.UserID); err != nil {
			return err
		}
		if !c.On {
			_, err := tx.Exec(`DELETE FROM follows WHERE user_id = ? AND post_id = ?`, c.UserID, c.PostID)
			return err
		}
		var status string
		if err := tx.QueryRow(`SELECT status FROM posts WHERE id = ?`, c.PostID).Scan(&status); err != nil {
			return notFoundGone(err)
		}
		if !shown(status) {
			return ErrGone
		}
		_, err := tx.Exec(`INSERT OR IGNORE INTO follows (user_id, post_id, created_at) VALUES (?, ?, ?)`, c.UserID, c.PostID, c.At)
		return err
	})
}

// MarkRead marks someone's notifications in a group read, up to and
// including UpTo (the newest one they were shown), so one that arrived
// while the page was open stays unread.
type MarkRead struct {
	GroupID int64
	UserID  int64
	UpTo    int64
	At      int64
}

func (c *MarkRead) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notifications SET read_at = ? WHERE user_id = ? AND id <= ? AND read_at IS NULL`,
			c.At, c.UserID, c.UpTo)
		return err
	})
}

// MarkEmailed records that notifications up to UpTo were sent by email, so
// the next pass doesn't send them again.
type MarkEmailed struct {
	GroupID int64
	UserID  int64
	UpTo    int64
	At      int64
}

func (c *MarkEmailed) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notifications SET emailed_at = ? WHERE user_id = ? AND id <= ? AND emailed_at IS NULL`,
			c.At, c.UserID, c.UpTo)
		return err
	})
}

// SetNotifyPrefs sets whether someone's notifications are also emailed,
// and whether they get the daily digest. Both are off until they choose.
type SetNotifyPrefs struct {
	UserID int64
	Email  bool
	Digest string // off | daily
	At     int64
}

func (c *SetNotifyPrefs) Apply(a *Applier) (any, error) {
	if c.Digest != DigestOff && c.Digest != DigestDaily {
		return nil, Invalid("the digest is off or daily")
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET notify_email = ?, digest = ? WHERE id = ?`, c.Email, c.Digest, c.UserID)
		return err
	})
}

// DigestSent records when someone's daily digest went out.
type DigestSent struct {
	UserID int64
	At     int64
}

func (c *DigestSent) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET digest_sent_at = ? WHERE id = ?`, c.At, c.UserID)
		return err
	})
}
