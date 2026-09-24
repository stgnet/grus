package cmd

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/stgnet/grus/internal/store"
)

// The group FAQ (plan section 4): topics and entries, each entry a question
// and what the group's threads say about it, with links to those threads.
//
// Entries are built from the link graph. Posts joined by link notes form a
// cluster; a cluster worth an entry (two or more linked posts, or one
// thread with real discussion) gets one from the nightly batch. When any
// source thread changes, its entries are marked stale, and the next nightly
// batch rewrites them (or answers "no change"). Mods are the editors: they
// can write, edit, move, hide and lock entries, and roll back any change.

// Limits on FAQ text. An answer is a short paragraph or two, not an essay.
const (
	MaxQuestionLen = 200
	MaxAnswerLen   = 2000
	MaxTopicLen    = 80
	// A cluster needs this many comments to be worth an entry on its own.
	FAQMinComments = 5
	// New entries per group per night: the batch runs while the Studio is
	// otherwise idle, and a group's whole history shouldn't land in one go.
	FAQNewPerNight = 20
)

// RootGroupID is the group file behind the root FAQ on the bare primary
// domain (plan section 4, "The root FAQ at nfb.group"). It's a group file
// like any other, so the same FAQ code runs it, but it's not in the groups
// table, so it has no address, no members, and no feed. Real group ids are
// time-based and far larger, so they never collide with it.
const RootGroupID = store.RootGroupID

// NewTopic asks CreateFAQEntry to file the entry under a topic that doesn't
// exist yet.
type NewTopic struct {
	ID       int64
	ParentID int64
	Title    string
}

// CreateTopic adds a topic to the outline (mods). A topic a mod created or
// named is locked: the weekly outline pass won't rename or merge it.
type CreateTopic struct {
	GroupID  int64
	TopicID  int64
	ParentID int64
	Title    string
	By       int64
	At       int64
}

func (c *CreateTopic) Apply(a *Applier) (any, error) {
	title, err := topicTitle(c.Title)
	if err != nil {
		return nil, err
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := insertTopic(tx, c.TopicID, c.ParentID, title, c.By != 0, c.At); err != nil {
			return err
		}
		return modLog(tx, c.By, "faq_topic_create", "faq_topic", c.TopicID, title, c.At)
	})
}

func topicTitle(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || len(s) > MaxTopicLen {
		return "", Invalid("a topic needs a title, up to %d characters", MaxTopicLen)
	}
	return s, nil
}

// insertTopic adds a topic. The outline is two levels deep ("Electrical ›
// House battery"), which is plenty for a group and keeps pages simple, so a
// parent must be a top-level topic.
func insertTopic(tx *sql.Tx, id, parent int64, title string, locked bool, at int64) error {
	if parent != 0 {
		var pp int64
		var merged sql.NullInt64
		if err := tx.QueryRow(`SELECT parent_id, merged_into FROM faq_topics WHERE id = ?`, parent).Scan(&pp, &merged); err != nil {
			return notFoundGone(err)
		}
		if pp != 0 || merged.Valid {
			return Invalid("topics go at most two levels deep")
		}
	}
	_, err := tx.Exec(`INSERT INTO faq_topics (id, parent_id, title, locked, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, parent, title, locked, at)
	return err
}

// liveTopic follows merges to the topic that holds a merged topic's
// entries now.
func liveTopic(tx *sql.Tx, id int64) (int64, error) {
	for range 10 { // merges don't chain deeply; this just can't loop forever
		var merged sql.NullInt64
		if err := tx.QueryRow(`SELECT merged_into FROM faq_topics WHERE id = ?`, id).Scan(&merged); err != nil {
			return 0, notFoundGone(err)
		}
		if !merged.Valid {
			return id, nil
		}
		id = merged.Int64
	}
	return id, nil
}

// EditTopic renames or moves a topic (mods), which also locks it.
type EditTopic struct {
	GroupID  int64
	TopicID  int64
	Title    string
	ParentID int64
	Sort     int64
	By       int64
	At       int64
}

func (c *EditTopic) Apply(a *Applier) (any, error) {
	title, err := topicTitle(c.Title)
	if err != nil {
		return nil, err
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var old string
		if err := tx.QueryRow(`SELECT title FROM faq_topics WHERE id = ? AND merged_into IS NULL`, c.TopicID).Scan(&old); err != nil {
			return notFoundGone(err)
		}
		if c.ParentID != 0 {
			var pp int64
			var kids int
			if err := tx.QueryRow(`SELECT parent_id FROM faq_topics WHERE id = ? AND merged_into IS NULL`, c.ParentID).Scan(&pp); err != nil {
				return notFoundGone(err)
			}
			tx.QueryRow(`SELECT COUNT(*) FROM faq_topics WHERE parent_id = ? AND merged_into IS NULL`, c.TopicID).Scan(&kids)
			if pp != 0 || kids > 0 || c.ParentID == c.TopicID {
				return Invalid("topics go at most two levels deep")
			}
		}
		if _, err := tx.Exec(`UPDATE faq_topics SET title = ?, parent_id = ?, sort = ?, locked = 1 WHERE id = ?`,
			title, c.ParentID, c.Sort, c.TopicID); err != nil {
			return err
		}
		return modLog(tx, c.By, "faq_topic_edit", "faq_topic", c.TopicID, old+" → "+title, c.At)
	})
}

// CreateFAQEntry adds an entry. By is the mod who wrote it, or 0 when it
// comes from a faq_new job (then JobID, Worker and Version are set, and
// Version is the anchor post's thread version).
type CreateFAQEntry struct {
	GroupID  int64
	EntryID  int64
	TopicID  int64     // an existing topic, or 0 with NewTopic
	NewTopic *NewTopic `json:",omitempty"`
	Question string
	Answer   string
	Posts    []int64 // source threads
	Sources  []int64 // outside sources it cites
	By       int64
	JobID    int64
	Worker   string
	Version  int64
	At       int64
}

func (c *CreateFAQEntry) Apply(a *Applier) (any, error) {
	q, ans, textErr := entryText(c.Question, c.Answer)
	if textErr != nil && c.By != 0 {
		return nil, textErr
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if c.By == 0 {
			// From the nightly batch: the usual stale-result check, against
			// the thread the cluster was found from (Posts[0], the anchor).
			// The job closes either way; an empty or unusable draft just
			// writes nothing.
			var current int64
			if len(c.Posts) > 0 {
				tx.QueryRow(`SELECT thread_version FROM posts WHERE id = ?`, c.Posts[0]).Scan(&current)
			}
			ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
			if err != nil || !ok || textErr != nil {
				return err
			}
		}
		posts, err := readablePosts(tx, c.Posts)
		if err != nil {
			return err
		}
		if c.By == 0 {
			if len(posts) == 0 {
				return nil // the threads went away meanwhile
			}
			// Another entry may have taken some of these threads since the
			// job was queued. Then they join that entry instead of making a
			// near-duplicate, and it's rewritten with them at the next batch.
			var existing int64
			err := tx.QueryRow(`SELECT e.id FROM faq_sources s JOIN faq_entries e ON e.id = s.entry_id
				WHERE s.post_id IN (`+placeholders(len(posts))+`) AND e.status = 'active' LIMIT 1`, anys(posts)...).Scan(&existing)
			if err == nil {
				for _, p := range posts {
					if err := addFAQSource(tx, existing, p); err != nil {
						return err
					}
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		topic := c.TopicID
		if c.NewTopic != nil {
			title, err := topicTitle(c.NewTopic.Title)
			if err != nil {
				return err
			}
			// A topic the batch makes up is unlocked, so the weekly pass
			// can tidy it; one a mod names is locked.
			if err := insertTopic(tx, c.NewTopic.ID, c.NewTopic.ParentID, title, c.By != 0, c.At); err != nil {
				return err
			}
			topic = c.NewTopic.ID
		}
		if topic, err = liveTopic(tx, topic); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO faq_entries (id, topic_id, question, answer, created_at, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, c.EntryID, topic, q, ans, c.At, c.At, nullIfZero(c.By)); err != nil {
			return err
		}
		for _, p := range posts {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO faq_sources (entry_id, post_id) VALUES (?, ?)`, c.EntryID, p); err != nil {
				return err
			}
		}
		for _, s := range c.Sources {
			if err := linkSource(tx, c.GroupID, s, 0, c.EntryID, c.At); err != nil {
				return err
			}
		}
		if err := faqHistory(tx, c.EntryID, q, ans, c.By, c.At); err != nil {
			return err
		}
		if err := ftsPut(tx, "faq", c.EntryID, 0, q, ans); err != nil {
			return err
		}
		return modLog(tx, c.By, "faq_create", "faq_entry", c.EntryID, q, c.At)
	})
}

func entryText(q, ans string) (string, string, error) {
	q = strings.Join(strings.Fields(q), " ")
	ans = strings.TrimSpace(ans)
	if q == "" || len(q) > MaxQuestionLen {
		return "", "", Invalid("an entry needs a question, up to %d characters", MaxQuestionLen)
	}
	if ans == "" || len(ans) > MaxAnswerLen {
		return "", "", Invalid("an entry needs an answer, up to %d characters", MaxAnswerLen)
	}
	return q, ans, nil
}

// readablePosts keeps the posts that are still shown (visible or flagged),
// in the order given, without repeats.
func readablePosts(tx *sql.Tx, ids []int64) ([]int64, error) {
	var out []int64
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var status string
		err := tx.QueryRow(`SELECT status FROM posts WHERE id = ?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if status == "visible" || status == "flagged" {
			out = append(out, id)
		}
	}
	return out, nil
}

// faqHistory records a version of an entry as it was written. Every
// version is kept (the first one too), so rollback is choosing a row.
func faqHistory(tx *sql.Tx, entry int64, q, ans string, by, at int64) error {
	_, err := tx.Exec(`INSERT INTO faq_history (entry_id, question, answer, changed_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		entry, q, ans, nullIfZero(by), at)
	return err
}

// entryChanged marks an entry for a rewrite at the next nightly batch.
func entryChanged(tx *sql.Tx, entry int64) error {
	_, err := tx.Exec(`UPDATE faq_entries SET stale = 1, version = version + 1 WHERE id = ?`, entry)
	return err
}

// addFAQSource makes a post one of an entry's sources, if it isn't yet.
func addFAQSource(tx *sql.Tx, entry, post int64) error {
	res, err := tx.Exec(`INSERT OR IGNORE INTO faq_sources (entry_id, post_id) VALUES (?, ?)`, entry, post)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return entryChanged(tx, entry)
	}
	return nil
}

// AddFAQSource points a post at an entry: a repeat question the check job
// matched to it ("Covered in the FAQ"), or a mod's choice. The post becomes
// one of the entry's sources, so anything new its thread produces flows
// back into the entry at the next rewrite.
type AddFAQSource struct {
	GroupID int64
	EntryID int64
	PostID  int64
	By      int64
	At      int64
}

func (c *AddFAQSource) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM faq_entries WHERE id = ?`, c.EntryID).Scan(&status); err != nil {
			return notFoundGone(err)
		}
		posts, err := readablePosts(tx, []int64{c.PostID})
		if err != nil || len(posts) == 0 || status != "active" {
			return err
		}
		return addFAQSource(tx, c.EntryID, c.PostID)
	})
}

// RemoveFAQSource takes a thread out of an entry's sources (mods).
type RemoveFAQSource struct {
	GroupID int64
	EntryID int64
	PostID  int64
	By      int64
	At      int64
}

func (c *RemoveFAQSource) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM faq_sources WHERE entry_id = ? AND post_id = ?`, c.EntryID, c.PostID); err != nil {
			return err
		}
		if err := entryChanged(tx, c.EntryID); err != nil {
			return err
		}
		return modLog(tx, c.By, "faq_source_remove", "faq_entry", c.EntryID, "", c.At)
	})
}

// EditFAQEntry is a mod editing an entry: its text, its topic, whether the
// system may rewrite it (Locked), and whether it's shown (Hidden). Any
// suggestion waiting for a locked entry is cleared: the mod has decided.
type EditFAQEntry struct {
	GroupID  int64
	EntryID  int64
	TopicID  int64
	Question string
	Answer   string
	Locked   bool
	Hidden   bool
	By       int64
	At       int64
}

func (c *EditFAQEntry) Apply(a *Applier) (any, error) {
	q, ans, err := entryText(c.Question, c.Answer)
	if err != nil {
		return nil, err
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var oldQ, oldA string
		if err := tx.QueryRow(`SELECT question, answer FROM faq_entries WHERE id = ?`, c.EntryID).Scan(&oldQ, &oldA); err != nil {
			return notFoundGone(err)
		}
		topic, err := liveTopic(tx, c.TopicID)
		if err != nil {
			return err
		}
		status := "active"
		if c.Hidden {
			status = "hidden"
		}
		textChanged := q != oldQ || ans != oldA
		if _, err := tx.Exec(`UPDATE faq_entries SET topic_id = ?, question = ?, answer = ?, locked = ?, status = ?,
			suggestion = NULL, stale = CASE WHEN ? THEN 0 ELSE stale END,
			updated_at = CASE WHEN ? THEN ? ELSE updated_at END,
			updated_by = CASE WHEN ? THEN ? ELSE updated_by END
			WHERE id = ?`, topic, q, ans, c.Locked, status, textChanged, textChanged, c.At, textChanged, c.By, c.EntryID); err != nil {
			return err
		}
		if textChanged {
			if err := faqHistory(tx, c.EntryID, q, ans, c.By, c.At); err != nil {
				return err
			}
		}
		if err := faqIndex(tx, c.EntryID); err != nil {
			return err
		}
		return modLog(tx, c.By, "faq_edit", "faq_entry", c.EntryID, q, c.At)
	})
}

// faqIndex puts an entry in the search index if it's shown, or takes it
// out if not.
func faqIndex(tx *sql.Tx, entry int64) error {
	var q, ans, status string
	if err := tx.QueryRow(`SELECT question, answer, status FROM faq_entries WHERE id = ?`, entry).Scan(&q, &ans, &status); err != nil {
		return notFoundGone(err)
	}
	if status != "active" {
		return ftsDel(tx, entry)
	}
	return ftsPut(tx, "faq", entry, 0, q, ans)
}

// RollbackFAQEntry puts an earlier version of an entry back: one tap to
// undo a bad rewrite (plan section 4). It's recorded as a new version, so
// the rollback can itself be undone.
type RollbackFAQEntry struct {
	GroupID   int64
	EntryID   int64
	HistoryID int64
	By        int64
	At        int64
}

func (c *RollbackFAQEntry) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var q, ans string
		if err := tx.QueryRow(`SELECT question, answer FROM faq_history WHERE id = ? AND entry_id = ?`,
			c.HistoryID, c.EntryID).Scan(&q, &ans); err != nil {
			return notFoundGone(err)
		}
		if _, err := tx.Exec(`UPDATE faq_entries SET question = ?, answer = ?, stale = 0, suggestion = NULL,
			updated_at = ?, updated_by = ? WHERE id = ?`, q, ans, c.At, c.By, c.EntryID); err != nil {
			return err
		}
		if err := faqHistory(tx, c.EntryID, q, ans, c.By, c.At); err != nil {
			return err
		}
		if err := faqIndex(tx, c.EntryID); err != nil {
			return err
		}
		return modLog(tx, c.By, "faq_rollback", "faq_entry", c.EntryID, q, c.At)
	})
}

// SetFAQAnswer is a faq_rewrite job's result. Sources that are no longer
// shown drop out first (removed and hidden posts leave an entry's sources
// at the next rewrite). A locked entry keeps its text and gets the rewrite
// as a suggestion for the mods instead. Every system change goes in the
// mod log and the entry's history, so a bad one is a one-tap rollback.
type SetFAQAnswer struct {
	GroupID  int64
	JobID    int64
	Worker   string
	EntryID  int64
	Version  int64
	Answer   string
	NoChange bool
	At       int64
}

func (c *SetFAQAnswer) Apply(a *Applier) (any, error) {
	ans := strings.TrimSpace(c.Answer)
	if len(ans) > MaxAnswerLen {
		ans = ans[:MaxAnswerLen]
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var current int64
		var q, old string
		var locked bool
		if err := tx.QueryRow(`SELECT version, question, answer, locked FROM faq_entries WHERE id = ?`, c.EntryID).
			Scan(&current, &q, &old, &locked); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
		if err != nil || !ok {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM faq_sources WHERE entry_id = ? AND post_id IN
			(SELECT id FROM posts WHERE status NOT IN ('visible', 'flagged'))`, c.EntryID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE faq_entries SET stale = 0 WHERE id = ?`, c.EntryID); err != nil {
			return err
		}
		if c.NoChange || ans == "" || ans == old {
			return nil
		}
		if locked {
			_, err := tx.Exec(`UPDATE faq_entries SET suggestion = ? WHERE id = ?`, ans, c.EntryID)
			return err
		}
		if _, err := tx.Exec(`UPDATE faq_entries SET answer = ?, updated_at = ?, updated_by = NULL WHERE id = ?`,
			ans, c.At, c.EntryID); err != nil {
			return err
		}
		if err := faqHistory(tx, c.EntryID, q, ans, 0, c.At); err != nil {
			return err
		}
		if err := faqIndex(tx, c.EntryID); err != nil {
			return err
		}
		return modLog(tx, 0, "faq_rewrite", "faq_entry", c.EntryID, q, c.At)
	})
}

// AddFAQComment is a member's comment on an entry. It's new information
// for the entry, the same as a new thread: the entry is rewritten with it
// at the next batch.
type AddFAQComment struct {
	GroupID   int64
	CommentID int64
	EntryID   int64
	UserID    int64
	Body      string
	At        int64
}

func (c *AddFAQComment) Apply(a *Applier) (any, error) {
	body := strings.TrimSpace(c.Body)
	if body == "" || len(body) > MaxCommentLen {
		return nil, Invalid("a comment needs text, up to %d characters", MaxCommentLen)
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := requireMember(tx, c.UserID); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(`SELECT status FROM faq_entries WHERE id = ?`, c.EntryID).Scan(&status); err != nil {
			return notFoundGone(err)
		}
		if status != "active" {
			return ErrGone
		}
		if _, err := tx.Exec(`INSERT INTO faq_comments (id, entry_id, user_id, body, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.CommentID, c.EntryID, c.UserID, body, c.At); err != nil {
			return err
		}
		return entryChanged(tx, c.EntryID)
	})
}

// RemoveFAQComment hides a comment on an entry: its author deleting it, or
// a mod removing it. Like any deletion it's purged later.
type RemoveFAQComment struct {
	GroupID   int64
	CommentID int64
	By        int64
	ByMod     bool
	At        int64
}

func (c *RemoveFAQComment) Apply(a *Applier) (any, error) {
	status, keep := "deleted", int64(AuthorDeleteKeep)
	if c.ByMod {
		status, keep = "removed", ModRemoveKeep
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var entry int64
		if err := tx.QueryRow(`SELECT entry_id FROM faq_comments WHERE id = ?`, c.CommentID).Scan(&entry); err != nil {
			return notFoundGone(err)
		}
		if _, err := tx.Exec(`UPDATE faq_comments SET status = ?, purge_after = ? WHERE id = ?`,
			status, c.At+keep, c.CommentID); err != nil {
			return err
		}
		if err := entryChanged(tx, entry); err != nil {
			return err
		}
		if c.ByMod {
			return modLog(tx, c.By, "remove", "faq_comment", c.CommentID, "", c.At)
		}
		return nil
	})
}

// SetPostTopics is the author or a mod choosing a post's topics. From then
// on they're the post's own: the system stops tagging it.
type SetPostTopics struct {
	GroupID int64
	PostID  int64
	Topics  []int64
	By      int64
	ByMod   bool
	At      int64
}

func (c *SetPostTopics) Apply(a *Applier) (any, error) {
	if len(c.Topics) > 3 {
		return nil, Invalid("a post has at most three topics")
	}
	source := "author"
	if c.ByMod {
		source = "mod"
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE posts SET topics_manual = 1 WHERE id = ?`, c.PostID); err != nil {
			return err
		}
		return setTopics(tx, c.PostID, c.Topics, source, true)
	})
}

// setTopics replaces a post's topic tags. The system (source "auto") only
// replaces its own tags, and not at all once a person has set them.
func setTopics(tx *sql.Tx, post int64, topics []int64, source string, byPerson bool) error {
	if !byPerson {
		var manual bool
		if err := tx.QueryRow(`SELECT topics_manual FROM posts WHERE id = ?`, post).Scan(&manual); err != nil {
			return notFoundGone(err)
		}
		if manual {
			return nil
		}
	}
	if _, err := tx.Exec(`DELETE FROM post_topics WHERE post_id = ?`, post); err != nil {
		return err
	}
	n := 0
	for _, t := range topics {
		live, err := liveTopic(tx, t)
		if err != nil {
			if IsInput(err) {
				continue // a topic merged or gone meanwhile
			}
			return err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO post_topics (post_id, topic_id, source) VALUES (?, ?, ?)`, post, live, source)
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 1 {
			n++
		}
		if n == 3 {
			break
		}
	}
	return nil
}

// TopicMerge folds topic Drop into Keep.
type TopicMerge struct{ Keep, Drop int64 }

// TopicRename gives a topic a clearer title.
type TopicRename struct {
	ID    int64
	Title string
}

// TidyTopics is the weekly outline pass's result: duplicate topics merged,
// vague ones renamed. Topics a mod created or named are left alone.
type TidyTopics struct {
	GroupID int64
	JobID   int64
	Worker  string
	Merges  []TopicMerge
	Renames []TopicRename
	At      int64
}

func (c *TidyTopics) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		ok, err := finishJob(tx, c.JobID, c.Worker, 0, 0, c.At)
		if err != nil || !ok {
			return err
		}
		lockedOrGone := func(id int64) bool {
			var locked bool
			var merged sql.NullInt64
			err := tx.QueryRow(`SELECT locked, merged_into FROM faq_topics WHERE id = ?`, id).Scan(&locked, &merged)
			return err != nil || locked || merged.Valid
		}
		for _, m := range c.Merges {
			if m.Keep == m.Drop || lockedOrGone(m.Drop) {
				continue
			}
			if keep, err := liveTopic(tx, m.Keep); err != nil || keep == m.Drop {
				continue
			}
			var kp, dp int64
			tx.QueryRow(`SELECT parent_id FROM faq_topics WHERE id = ?`, m.Keep).Scan(&kp)
			tx.QueryRow(`SELECT parent_id FROM faq_topics WHERE id = ?`, m.Drop).Scan(&dp)
			var kids int
			tx.QueryRow(`SELECT COUNT(*) FROM faq_topics WHERE parent_id = ? AND merged_into IS NULL`, m.Drop).Scan(&kids)
			if kp == m.Drop || (kids > 0 && kp != 0) {
				continue // would break the two-level outline
			}
			for _, q := range []string{
				`UPDATE faq_entries SET topic_id = ?1 WHERE topic_id = ?2`,
				`INSERT OR IGNORE INTO post_topics (post_id, topic_id, source) SELECT post_id, ?1, source FROM post_topics WHERE topic_id = ?2`,
				`DELETE FROM post_topics WHERE topic_id = ?2`,
				`UPDATE faq_topics SET parent_id = ?1 WHERE parent_id = ?2 AND merged_into IS NULL`,
				`UPDATE faq_topics SET merged_into = ?1 WHERE id = ?2`,
			} {
				if _, err := tx.Exec(q, m.Keep, m.Drop); err != nil {
					return err
				}
			}
			if err := modLog(tx, 0, "faq_topic_merge", "faq_topic", m.Drop, "", c.At); err != nil {
				return err
			}
		}
		for _, r := range c.Renames {
			title, err := topicTitle(r.Title)
			if err != nil || lockedOrGone(r.ID) {
				continue
			}
			var old string
			tx.QueryRow(`SELECT title FROM faq_topics WHERE id = ?`, r.ID).Scan(&old)
			if old == title {
				continue
			}
			if _, err := tx.Exec(`UPDATE faq_topics SET title = ? WHERE id = ?`, title, r.ID); err != nil {
				return err
			}
			if err := modLog(tx, 0, "faq_topic_rename", "faq_topic", r.ID, old+" → "+title, c.At); err != nil {
				return err
			}
		}
		return nil
	})
}

// QueueFAQ is the nightly FAQ batch (plan section 4, "How it stays
// current"). The leader submits one a night, when the Studio is otherwise
// idle; Weekly is set once a week. For every group with AI on, it queues:
//
//   - a rewrite for every stale entry;
//   - a new entry for every cluster of linked threads that no entry covers
//     yet and that's worth one (FAQNewPerNight at most);
//   - weekly: the outline pass, and a re-check of every outside page that
//     hasn't been read for a week.
//
// It only queues. The work itself is ordinary jobs, each short, claimed
// and version-checked like any other.
type QueueFAQ struct {
	Weekly bool
	At     int64
}

func (c *QueueFAQ) Apply(a *Applier) (any, error) {
	// It's on the site log, which knows every group; each group's part
	// runs in that group's own log.
	return nil, a.Site(func(tx *sql.Tx) error {
		groups, err := groupIDs(tx, `WHERE status = 'active'`)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if err := send(tx, &QueueGroupFAQ{GroupID: g, Weekly: c.Weekly, At: c.At}, c.At); err != nil {
				return err
			}
		}
		return nil
	})
}

// QueueGroupFAQ is one group's part of the nightly batch.
type QueueGroupFAQ struct {
	GroupID int64
	Weekly  bool
	At      int64
}

func (c *QueueGroupFAQ) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var on bool
		if err := tx.QueryRow(`SELECT ai_enabled FROM settings WHERE id = 1`).Scan(&on); err != nil || !on {
			return err
		}
		return queueFAQ(tx, c.Weekly, c.At)
	})
}

func queueFAQ(tx *sql.Tx, weekly bool, at int64) error {
	type ref struct{ id, version int64 }
	collect := func(q string, args ...any) ([]ref, error) {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []ref
		for rows.Next() {
			var r ref
			if err := rows.Scan(&r.id, &r.version); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	stale, err := collect(`SELECT id, version FROM faq_entries WHERE stale = 1 AND status = 'active'`)
	if err != nil {
		return err
	}
	for _, e := range stale {
		if err := schedule(tx, JobFAQRewrite, e.id, e.version, at, at); err != nil {
			return err
		}
	}
	clusters, err := uncoveredClusters(tx)
	if err != nil {
		return err
	}
	for i, cl := range clusters {
		if i == FAQNewPerNight {
			break
		}
		var v int64
		tx.QueryRow(`SELECT thread_version FROM posts WHERE id = ?`, cl[0]).Scan(&v)
		if err := schedule(tx, JobFAQNew, cl[0], v, at, at); err != nil {
			return err
		}
	}
	if !weekly {
		return nil
	}
	if err := schedule(tx, JobOutline, 0, 0, at, at); err != nil {
		return err
	}
	due, err := collect(`SELECT id, version FROM sources WHERE status IN ('active', 'gone') AND via != 'described'
		AND COALESCE(checked_at, 0) < ?`, at-6*24*3600)
	if err != nil {
		return err
	}
	for _, s := range due {
		if err := schedule(tx, JobSource, s.id, s.version, at, at); err != nil {
			return err
		}
	}
	return nil
}

// uncoveredClusters finds groups of linked threads that no active entry
// covers yet and that are worth an entry: two or more linked posts, or one
// thread with at least FAQMinComments comments. Each cluster is its post
// ids, oldest first (the first is the anchor its job is keyed by);
// clusters come oldest first too, so a group's history fills in in order.
// Threads without a digest yet are left for a later night: the entry is
// written from digests.
func uncoveredClusters(tx *sql.Tx) ([][]int64, error) {
	rows, err := tx.Query(`SELECT id, comment_count, digest IS NOT NULL,
		EXISTS (SELECT 1 FROM faq_sources s JOIN faq_entries e ON e.id = s.entry_id WHERE s.post_id = posts.id AND e.status = 'active')
		FROM posts WHERE status = 'visible' AND continues_post_id IS NULL`)
	if err != nil {
		return nil, err
	}
	type info struct {
		comments  int
		digested  bool
		inEntry   bool
		parent    int64
		component []int64
	}
	posts := map[int64]*info{}
	for rows.Next() {
		var id int64
		var in info
		if err := rows.Scan(&id, &in.comments, &in.digested, &in.inEntry); err != nil {
			rows.Close()
			return nil, err
		}
		in.parent = id
		posts[id] = &in
	}
	rows.Close()
	// Union-find over active links between shown posts.
	var find func(int64) int64
	find = func(x int64) int64 {
		for posts[x].parent != x {
			posts[x].parent = posts[posts[x].parent].parent
			x = posts[x].parent
		}
		return x
	}
	lrows, err := tx.Query(`SELECT older_post_id, newer_post_id FROM post_links WHERE state = 'active'`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var a, b int64
		if err := lrows.Scan(&a, &b); err != nil {
			lrows.Close()
			return nil, err
		}
		if posts[a] == nil || posts[b] == nil {
			continue
		}
		ra, rb := find(a), find(b)
		if ra != rb {
			posts[max(ra, rb)].parent = min(ra, rb)
		}
	}
	lrows.Close()
	groups := map[int64][]int64{}
	for id := range posts {
		r := find(id)
		groups[r] = append(groups[r], id)
	}
	var out [][]int64
	for _, ids := range groups {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		covered, ready, comments := false, true, 0
		for _, id := range ids {
			in := posts[id]
			covered = covered || in.inEntry
			ready = ready && in.digested
			comments += in.comments
		}
		if covered || !ready || len(ids) < 2 && comments < FAQMinComments {
			continue
		}
		out = append(out, ids)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
