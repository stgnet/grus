package cmd

import (
	"database/sql"
	"fmt"
)

// Purge hard-deletes everything whose retention has run out (plan section 6,
// "Deleting: hidden now, purged later"). The leader submits one a day with
// Before set to the current time; because Before is in the command, every
// node deletes exactly the same rows.
//
// Anything under legal_hold is kept until the hold is lifted.
//
// Not yet: the erasure list's bookkeeping.
type Purge struct {
	Before int64
}

func (c *Purge) Apply(a *Applier) (any, error) {
	// site.db: spent sign-ins, expired sessions, and the personal details of
	// accounts deleted longer ago than the grace period. The account row
	// stays (posts point at it and show "[deleted]"); what identifies the
	// person is removed.
	err := a.Site(func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM login_tokens WHERE expires_at < ?1`,
			`DELETE FROM sessions WHERE expires_at < ?1`,
			`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE purge_after < ?1)`,
			`UPDATE users SET email = NULL, handle = NULL, phone = NULL, photo_key = NULL, bio = NULL,
			        purge_after = NULL
			 WHERE purge_after < ?1`,
		} {
			if _, err := tx.Exec(q, c.Before); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Each group's file. The list comes from site.db, which is in the same
	// state on every node at this point in the log.
	rows, err := a.Store.Site().Query(`SELECT id FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var groups []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, id)
	}
	rows.Close()

	for _, id := range groups {
		err := a.Group(id, func(tx *sql.Tx) error {
			for _, q := range []string{
				// Comments that expired, and comments on posts that expired,
				// unless held.
				`DELETE FROM comments WHERE legal_hold = 0 AND (purge_after < ?1 OR post_id IN
				   (SELECT id FROM posts WHERE purge_after < ?1 AND legal_hold = 0))`,
				// Expired posts, unless held or still holding a held comment.
				`DELETE FROM posts WHERE purge_after < ?1 AND legal_hold = 0
				   AND NOT EXISTS (SELECT 1 FROM comments WHERE comments.post_id = posts.id)`,
				// Old versions: expired ones, unless their item is held; and any
				// whose item is gone.
				`DELETE FROM revisions WHERE
				   (purge_after < ?1
				     AND NOT (kind = 'post' AND ref_id IN (SELECT id FROM posts WHERE legal_hold = 1))
				     AND NOT (kind = 'comment' AND ref_id IN (SELECT id FROM comments WHERE legal_hold = 1)))
				   OR (kind = 'post' AND ref_id NOT IN (SELECT id FROM posts))
				   OR (kind = 'comment' AND ref_id NOT IN (SELECT id FROM comments))`,
				// Photo placements on items that are gone. The blob files
				// themselves are removed by each node's blob GC once nothing
				// references them.
				`DELETE FROM images WHERE post_id NOT IN (SELECT id FROM posts)
				   OR (comment_id IS NOT NULL AND comment_id NOT IN (SELECT id FROM comments))`,
				// Search rows, links, notes and queued AI work for posts
				// that are gone. (A deleted post's notes already stopped
				// showing when it was deleted.)
				`DELETE FROM search_fts WHERE post_id NOT IN (SELECT id FROM posts)`,
				`DELETE FROM post_links WHERE older_post_id NOT IN (SELECT id FROM posts)
				   OR newer_post_id NOT IN (SELECT id FROM posts)`,
				// (Sources in other groups, from M4, are checked against
				// that group's own posts, when it purges.)
				// (Rows for an outside source have post_id 0, and are kept.)
				fmt.Sprintf(`DELETE FROM note_sources WHERE (group_id = %d AND source_id = 0 AND post_id NOT IN (SELECT id FROM posts))
				   OR (comment_id != 0 AND comment_id NOT IN (SELECT id FROM comments))
				   OR note_id IN (SELECT id FROM notes WHERE host_post_id NOT IN (SELECT id FROM posts))`, id),
				`DELETE FROM notes WHERE host_post_id NOT IN (SELECT id FROM posts)
				   OR (kind = 'link' AND id NOT IN (SELECT note_id FROM note_sources))`,
				`DELETE FROM jobs WHERE (kind IN ('check', 'digest', 'summary', 'faq_new') AND ref_id NOT IN (SELECT id FROM posts))
				   OR (kind = 'note' AND ref_id NOT IN (SELECT id FROM notes))
				   OR (kind = 'check_comment' AND ref_id NOT IN (SELECT id FROM comments))`,
				// M3: the FAQ's links to posts that are gone, topic tags,
				// nudges, and outside pages shown on them; and comments on
				// FAQ entries past their retention.
				`DELETE FROM faq_sources WHERE post_id NOT IN (SELECT id FROM posts)`,
				`DELETE FROM post_topics WHERE post_id NOT IN (SELECT id FROM posts)`,
				`DELETE FROM nudges WHERE post_id NOT IN (SELECT id FROM posts)`,
				`DELETE FROM source_links WHERE post_id != 0 AND post_id NOT IN (SELECT id FROM posts)`,
				`DELETE FROM sister_links WHERE post_id NOT IN (SELECT id FROM posts)`,
				// M5: reports and votes on items that are gone.
				`DELETE FROM reports WHERE (kind = 'post' AND item_id NOT IN (SELECT id FROM posts))
				   OR (kind = 'comment' AND item_id NOT IN (SELECT id FROM comments))`,
				`DELETE FROM flag_votes WHERE (kind = 'post' AND item_id NOT IN (SELECT id FROM posts))
				   OR (kind = 'comment' AND item_id NOT IN (SELECT id FROM comments))`,
				`DELETE FROM faq_comments WHERE purge_after < ?1`,
				`UPDATE posts SET continues_post_id = NULL, continued_at = NULL
				   WHERE continues_post_id IS NOT NULL AND continues_post_id NOT IN (SELECT id FROM posts)`,
				// "Not what I was looking for" searches are kept 180 days.
				`DELETE FROM ai_feedback WHERE created_at < ?1 - 180 * 86400`,
			} {
				if _, err := tx.Exec(q, c.Before); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}
