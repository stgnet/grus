package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Reads for the AI side: the job queue, notes, links, and threads prepared
// for the model.

// Job is one row of a group's job queue.
type Job struct {
	ID         int64
	Kind       string
	RefID      int64
	RefVersion int64
}

// DueJobs lists jobs ready to run in a group (not done, due, not leased),
// oldest first.
func (s *Store) DueJobs(groupID, now int64, limit int) ([]Job, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, kind, ref_id, ref_version FROM jobs
		WHERE done_at IS NULL AND run_after <= ? AND (lease_until IS NULL OR lease_until < ?)
		ORDER BY run_after, id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.RefID, &j.RefVersion); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// QueueDepth counts jobs waiting (due now) and scheduled (later) across
// every group, for the admin page.
func (s *Store) QueueDepth(now int64) (due, later int, err error) {
	ids, err := s.GroupFileIDs()
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		db, err := s.Group(id)
		if err != nil {
			return 0, 0, err
		}
		var d, l int
		db.QueryRow(`SELECT COALESCE(SUM(run_after <= ?1), 0), COALESCE(SUM(run_after > ?1), 0)
			FROM jobs WHERE done_at IS NULL`, now).Scan(&d, &l)
		due += d
		later += l
	}
	return due, later, nil
}

// Note is a system-written text in a thread, as pages show it.
type Note struct {
	ID             int64
	HostPostID     int64
	AfterCommentID int64
	Kind           string
	Text           string
	Stale          bool
	UpdatedAt      int64
	// For link notes: the post it points at.
	SourceGroupID int64
	SourcePostID  int64
}

// Notes lists a thread's active notes, oldest first.
func (s *Store) Notes(groupID, hostPostID int64) ([]Note, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT n.id, n.host_post_id, n.after_comment_id, n.kind, n.text, n.stale, n.updated_at,
		COALESCE((SELECT group_id FROM note_sources WHERE note_id = n.id LIMIT 1), 0),
		COALESCE((SELECT post_id FROM note_sources WHERE note_id = n.id LIMIT 1), 0)
		FROM notes n WHERE n.host_post_id = ? AND n.state = 'active' ORDER BY n.created_at, n.id`, hostPostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.HostPostID, &n.AfterCommentID, &n.Kind, &n.Text, &n.Stale, &n.UpdatedAt,
			&n.SourceGroupID, &n.SourcePostID); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Note reads one note.
func (s *Store) Note(groupID, id int64) (*Note, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	var n Note
	err = db.QueryRow(`SELECT n.id, n.host_post_id, n.after_comment_id, n.kind, n.text, n.stale, n.updated_at,
		COALESCE((SELECT group_id FROM note_sources WHERE note_id = n.id LIMIT 1), 0),
		COALESCE((SELECT post_id FROM note_sources WHERE note_id = n.id LIMIT 1), 0)
		FROM notes n WHERE n.id = ? AND n.state = 'active'`, id).Scan(&n.ID, &n.HostPostID, &n.AfterCommentID, &n.Kind,
		&n.Text, &n.Stale, &n.UpdatedAt, &n.SourceGroupID, &n.SourcePostID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// NoteVersion matches cmd's noteVersion: the sum of the note's sources'
// thread versions. A worker records it before reading the sources.
func (s *Store) NoteVersion(groupID, noteID int64) (int64, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return 0, err
	}
	var v int64
	err = db.QueryRow(`SELECT COALESCE(SUM(p.thread_version), 0) FROM note_sources s JOIN posts p ON p.id = s.post_id
		WHERE s.note_id = ?`, noteID).Scan(&v)
	return v, err
}

// LinkedPosts lists the posts linked to a post (active links), for Ask to
// follow a chain of threads across years.
func (s *Store) LinkedPosts(groupID, postID int64) ([]int64, error) {
	return s.int64s(groupID, `SELECT CASE WHEN older_post_id = ?1 THEN newer_post_id ELSE older_post_id END
		FROM post_links WHERE (older_post_id = ?1 OR newer_post_id = ?1) AND state = 'active'`, postID)
}

// LinkPairs lists every post a post has been paired with, active or
// rejected, so the check job doesn't propose a pair again.
func (s *Store) LinkPairs(groupID, postID int64) (map[int64]bool, error) {
	ids, err := s.int64s(groupID, `SELECT CASE WHEN older_post_id = ?1 THEN newer_post_id ELSE older_post_id END
		FROM post_links WHERE older_post_id = ?1 OR newer_post_id = ?1`, postID)
	m := map[int64]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m, err
}

// Updates lists the posts moved under a post as Update sections, oldest
// first.
func (s *Store) Updates(groupID, postID int64) ([]Post, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+postCols+` FROM posts WHERE continues_post_id = ? ORDER BY created_at, id`, postID)
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

// PostsByUser lists a user's recent visible posts in a group that stand on
// their own, newest first: the choices for "move under an earlier post".
func (s *Store) PostsByUser(groupID, userID, before int64, limit int) ([]Post, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+postCols+` FROM posts WHERE user_id = ? AND id < ? AND status IN ('visible', 'flagged')
		AND continues_post_id IS NULL ORDER BY id DESC LIMIT ?`, userID, before, limit)
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

func (s *Store) int64s(groupID int64, q string, args ...any) ([]int64, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(q, args...)
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

// ThreadText is a thread written out for the model: title, the post, and
// its readable comments, with authors as "OP" and "commenter N" and never
// a handle or id (plan section 3, privacy rule 3). An anonymous post reads
// exactly like any other. Updates moved under the post are included.
// maxChars keeps a very long thread within the model's budget: the start
// of the thread is kept, where the question and the first answers are, plus
// the most recent comments, where fixes tend to be reported.
func (s *Store) ThreadText(groupID, postID int64, maxChars int) (string, *Post, error) {
	p, err := s.Post(groupID, postID)
	if err != nil || p == nil {
		return "", p, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\nPosted: %s\n", p.Title, ymd(p.CreatedAt))
	if p.Body != "" {
		fmt.Fprintf(&b, "OP: %s\n", oneParagraph(p.Body))
	}
	posts := []Post{*p}
	ups, err := s.Updates(groupID, postID)
	if err != nil {
		return "", nil, err
	}
	for _, u := range ups {
		if u.Status == "visible" || u.Status == "flagged" {
			fmt.Fprintf(&b, "Update from OP (%s): %s\n%s\n", ymd(u.CreatedAt), u.Title, oneParagraph(u.Body))
			posts = append(posts, u)
		}
	}
	// Label authors consistently across the whole thread.
	labels := map[int64]string{}
	if p.UserID != 0 {
		labels[p.UserID] = "OP"
	}
	next := 1
	newLabel := func() string {
		l := fmt.Sprintf("commenter %d", next)
		next++
		return l
	}
	var lines []string
	for _, tp := range posts {
		cs, err := s.Comments(groupID, tp.ID)
		if err != nil {
			return "", nil, err
		}
		for _, c := range cs {
			if c.Status != "visible" && c.Status != "flagged" {
				continue
			}
			var who string
			switch {
			case c.UserID == 0:
				// Archive comments have no account, so there's no telling
				// who's who: each is labeled as its own person.
				who = newLabel()
			case labels[c.UserID] != "":
				who = labels[c.UserID]
			default:
				labels[c.UserID] = newLabel()
				who = labels[c.UserID]
			}
			reply := ""
			if c.ParentID != 0 {
				reply = " (reply)"
			}
			lines = append(lines, fmt.Sprintf("%s%s, %s: %s", who, reply, ymd(c.CreatedAt), oneParagraph(c.Body)))
		}
	}
	head := b.String()
	budget := maxChars - len(head)
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	if total > budget && len(lines) > 4 {
		// Keep whole comments from both ends until the budget is used.
		var first, last []string
		used := 0
		for i, j := 0, len(lines)-1; i <= j; {
			if len(first) <= len(last) {
				if used+len(lines[i]) > budget {
					break
				}
				used += len(lines[i]) + 1
				first = append(first, lines[i])
				i++
			} else {
				if used+len(lines[j]) > budget {
					break
				}
				used += len(lines[j]) + 1
				last = append([]string{lines[j]}, last...)
				j--
			}
		}
		skipped := len(lines) - len(first) - len(last)
		lines = append(append(first, fmt.Sprintf("[%d comments not shown]", skipped)), last...)
	}
	return head + strings.Join(lines, "\n"), p, nil
}

func ymd(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02") }

// oneParagraph folds line breaks so every comment is one line of the
// transcript, and trims very long ones.
func oneParagraph(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 2000 {
		s = s[:2000] + "…"
	}
	return s
}

// UsageRow is one purpose's AI usage on one day, summed over nodes.
type UsageRow struct {
	Day          string
	Purpose      string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	Seconds      float64
	Failures     int64
}

// AIUsage lists usage since a day (YYYY-MM-DD), newest day first.
func (s *Store) AIUsage(since string) ([]UsageRow, error) {
	rows, err := s.Site().Query(`SELECT day, purpose, SUM(calls), SUM(input_tokens), SUM(output_tokens), SUM(seconds), SUM(failures)
		FROM ai_usage WHERE day >= ? GROUP BY day, purpose ORDER BY day DESC, purpose`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Day, &u.Purpose, &u.Calls, &u.InputTokens, &u.OutputTokens, &u.Seconds, &u.Failures); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
