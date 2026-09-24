package cmd

import (
	"database/sql"
	"errors"
	"strings"
)

// Moderation (plan section 6): roles, lock and pin, bans, the AI check's
// verdicts, member reports and votes, and the mod queue's approve.
//
// Item statuses, and who sees what (auth.CanRead has the rule):
//
//	visible      everyone who can read the group
//	flagged      the same, collapsed to one line, with a Keep / Hide vote
//	auto_hidden  hidden by the AI check or a vote; the author and mods see it
//	held         a newcomer's first post, waiting for a mod
//	removed      removed by a mod (SoftDelete)
//	deleted      deleted by its author (SoftDelete)
//
// "Shown" (visible or flagged) is what's searchable, counted in a post's
// comments, and allowed as the source of notes in sister groups. Every
// change between statuses goes through changeStatus, which keeps those in
// step.

// Moderation verdicts from the AI check.
const (
	VerdictClear      = "clear"
	VerdictBorderline = "borderline" // flagged: stays up, goes to a member vote
	VerdictViolation  = "violation"  // hidden now, goes to the mod queue
)

// Categories a verdict can give.
var Categories = []string{"mean", "off_topic", "spam", "unsafe"}

// MaxExamples is how many recent mod decisions go into each check.
const MaxExamples = 20

// MemberVoteAge is how long someone must have been a member to vote on a
// flagged item, so sock puppets and brand-new accounts can't swing it.
const MemberVoteAge = 30 * 86400

func shown(status string) bool { return status == "visible" || status == "flagged" }

// item is what moderation needs to know about a post or comment.
type item struct {
	kind      string
	id        int64
	postID    int64 // the post, or the comment's post
	author    int64
	parentID  int64 // a reply's top comment
	status    string
	title     string
	body      string
	aiCleared bool
}

func loadItem(tx *sql.Tx, kind string, id int64) (*item, error) {
	it := &item{kind: kind, id: id}
	var author, parent sql.NullInt64
	var err error
	switch kind {
	case "post":
		it.postID = id
		err = tx.QueryRow(`SELECT user_id, status, title, body, ai_cleared FROM posts WHERE id = ?`, id).
			Scan(&author, &it.status, &it.title, &it.body, &it.aiCleared)
	case "comment":
		err = tx.QueryRow(`SELECT post_id, user_id, parent_id, status, body, ai_cleared FROM comments WHERE id = ?`, id).
			Scan(&it.postID, &author, &parent, &it.status, &it.body, &it.aiCleared)
	default:
		return nil, Invalid("unknown kind %q", kind)
	}
	if err != nil {
		return nil, notFoundGone(err)
	}
	it.author, it.parentID = author.Int64, parent.Int64
	return it, nil
}

func (it *item) table() string {
	if it.kind == "post" {
		return "posts"
	}
	return "comments"
}

// changeStatus moves an item between visible, flagged, auto_hidden and
// held, keeping the search index, the post's comment count, and the
// thread's derived work (digest, notes, FAQ) in step. For a post whose
// shown-ness changes, it sends the sister groups word to hide (or show
// again) their notes written from it, and returns those sister posts.
func changeStatus(tx *sql.Tx, groupID int64, it *item, to string, at int64) ([]sisterRef, error) {
	was := it.status
	if was == to {
		return nil, nil
	}
	if _, err := tx.Exec(`UPDATE `+it.table()+` SET status = ? WHERE id = ?`, to, it.id); err != nil {
		return nil, err
	}
	it.status = to
	if shown(to) {
		if err := ftsPut(tx, it.kind, it.id, it.postID, it.title, it.body); err != nil {
			return nil, err
		}
	} else if err := ftsDel(tx, it.id); err != nil {
		return nil, err
	}
	if it.kind == "comment" && shown(was) != shown(to) {
		delta := 1
		if !shown(to) {
			delta = -1
		}
		if _, err := tx.Exec(`UPDATE posts SET comment_count = comment_count + ? WHERE id = ?`, delta, it.postID); err != nil {
			return nil, err
		}
	}
	if err := threadChanged(tx, groupID, it.postID, at); err != nil {
		return nil, err
	}
	if to == "auto_hidden" {
		if err := authorNotice(tx, it, NoteHidden, 0, at); err != nil {
			return nil, err
		}
	}
	if it.kind == "post" && shown(was) != shown(to) {
		// Notes in sister groups written from this post go (or come
		// back) with it.
		refs, err := sisterRefs(tx, it.id)
		if err != nil {
			return nil, err
		}
		return refs, sendSisterNotes(tx, groupID, it.id, refs, shown(to), at)
	}
	return nil, nil
}

// setFlag records why an item is flagged or hidden (or clears it).
func setFlag(tx *sql.Tx, it *item, by, category, reason string) error {
	_, err := tx.Exec(`UPDATE `+it.table()+` SET flagged_by = ?, flag_category = ?, flag_reason = ? WHERE id = ?`,
		nullIfEmpty(by), nullIfEmpty(category), nullIfEmpty(reason), it.id)
	return err
}

// modExample keeps a mod's decision as an example for the AI check: an
// excerpt of the item and the decision, nothing about who wrote it.
func modExample(tx *sql.Tx, it *item, decision string, at int64) error {
	text := strings.Join(strings.Fields(strings.TrimSpace(it.title+"\n"+it.body)), " ")
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300]) + "…"
	}
	if text == "" {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO mod_examples (text, decision, created_at) VALUES (?, ?, ?)`, text, decision, at); err != nil {
		return err
	}
	// Only the newest are ever used; keep a few more than that.
	_, err := tx.Exec(`DELETE FROM mod_examples WHERE id NOT IN (SELECT id FROM mod_examples ORDER BY id DESC LIMIT ?)`, MaxExamples*2)
	return err
}

// resolveReports closes the open reports on an item.
func resolveReports(tx *sql.Tx, it *item, at int64) error {
	_, err := tx.Exec(`UPDATE reports SET resolved_at = ? WHERE kind = ? AND item_id = ? AND resolved_at IS NULL`, at, it.kind, it.id)
	return err
}

// applyVerdict acts on the AI check's verdict for an item (from SetCheck
// or SetCommentCheck). A member vote or a mod's "keep" (ai_cleared) is
// final: the AI doesn't flag that item again. The AI only flags and hides,
// never removes; everything it does is in the mod log with no actor.
func applyVerdict(tx *sql.Tx, groupID int64, kind string, id int64, verdict, category, reason string, at int64) ([]sisterRef, error) {
	if verdict != VerdictBorderline && verdict != VerdictViolation {
		return nil, nil // clear, or nothing the code understands: leave it
	}
	it, err := loadItem(tx, kind, id)
	if err != nil {
		return nil, err
	}
	if it.aiCleared || !shown(it.status) {
		return nil, nil
	}
	valid := false
	for _, c := range Categories {
		valid = valid || c == category
	}
	if !valid {
		category = ""
	}
	if r := []rune(strings.TrimSpace(reason)); len(r) > 200 {
		reason = string(r[:200])
	}
	to, action := "flagged", "flag"
	if verdict == VerdictViolation {
		to, action = "auto_hidden", "auto_hide"
	} else if it.status == "flagged" {
		return nil, nil // already flagged (a report got there first)
	}
	refs, err := changeStatus(tx, groupID, it, to, at)
	if err != nil {
		return nil, err
	}
	if err := setFlag(tx, it, "auto", category, reason); err != nil {
		return nil, err
	}
	return refs, modLog(tx, 0, action, kind, id, strings.TrimSpace(category+": "+reason), at)
}

// SetCommentCheck is a comment check job's result: the moderation verdict.
// (Posts get theirs through SetCheck, alongside the link matches.)
type SetCommentCheck struct {
	GroupID   int64
	JobID     int64
	Worker    string
	CommentID int64
	Version   int64
	Verdict   string
	Category  string
	Reason    string
	At        int64
}

func (c *SetCommentCheck) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRow(`SELECT version FROM comments WHERE id = ?`, c.CommentID).Scan(&current); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, current, c.At)
		if err != nil || !ok {
			return err
		}
		_, err = applyVerdict(tx, c.GroupID, "comment", c.CommentID, c.Verdict, c.Category, c.Reason, c.At)
		return err
	})
}

// Approve is a mod letting an item stand: a held first post, or anything
// the AI, a report, or a vote flagged or hid. It's shown normally from
// then on, and the AI won't flag it again. When it overrides the AI, the
// decision becomes an example for future checks.
type Approve struct {
	GroupID int64
	Kind    string
	ID      int64
	By      int64
	At      int64
}

func (c *Approve) Apply(a *Applier) (any, error) {
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		it, err := loadItem(tx, c.Kind, c.ID)
		if err != nil {
			return err
		}
		if it.status == "removed" || it.status == "deleted" {
			return Invalid("use Restore for a removed or deleted item")
		}
		var by sql.NullString
		tx.QueryRow(`SELECT flagged_by FROM `+it.table()+` WHERE id = ?`, c.ID).Scan(&by)
		wasHeld := it.status == "held"
		if _, err = changeStatus(tx, c.GroupID, it, "visible", c.At); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE `+it.table()+` SET ai_cleared = 1, flagged_by = NULL, flag_category = NULL, flag_reason = NULL
			WHERE id = ?`, c.ID); err != nil {
			return err
		}
		if err := resolveReports(tx, it, c.At); err != nil {
			return err
		}
		if by.String == "auto" {
			if err := modExample(tx, it, "keep", c.At); err != nil {
				return err
			}
		}
		action := "approve"
		if wasHeld {
			action = "approve_held"
			if err := authorNotice(tx, it, NoteApproved, c.By, c.At); err != nil {
				return err
			}
			// Its check found no links while it was held; look again now.
			if c.Kind == "post" {
				var v int64
				tx.QueryRow(`SELECT version FROM posts WHERE id = ?`, c.ID).Scan(&v)
				if err := schedule(tx, JobCheck, c.ID, v, c.At, c.At); err != nil {
					return err
				}
			}
		}
		return modLog(tx, c.By, action, c.Kind, c.ID, "", c.At)
	})
	return nil, err
}

// Report is a member reporting an item. It counts like an AI flag: the
// item goes to a member vote (unless members or a mod already cleared
// it), it gets a fresh AI check, and it shows in the mod queue.
type Report struct {
	GroupID int64
	Kind    string
	ID      int64
	UserID  int64
	Reason  string
	At      int64
}

func (c *Report) Apply(a *Applier) (any, error) {
	reason := strings.TrimSpace(c.Reason)
	if len(reason) > 500 {
		return nil, Invalid("a report's reason can be up to 500 characters")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if err := requireMember(tx, c.UserID); err != nil {
			return err
		}
		it, err := loadItem(tx, c.Kind, c.ID)
		if err != nil {
			return err
		}
		if it.author == c.UserID {
			return Invalid("that's your own")
		}
		if _, err := tx.Exec(`INSERT INTO reports (kind, item_id, user_id, reason, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (kind, item_id, user_id) DO UPDATE SET reason = excluded.reason, resolved_at = NULL`, c.Kind, c.ID, c.UserID, reason, c.At); err != nil {
			return err
		}
		if it.status == "visible" && !it.aiCleared {
			if _, err := changeStatus(tx, c.GroupID, it, "flagged", c.At); err != nil {
				return err
			}
			if err := setFlag(tx, it, "report", "", reason); err != nil {
				return err
			}
		}
		// A fresh check, with the report's reason in the queue beside it.
		if c.Kind == "post" {
			var v int64
			tx.QueryRow(`SELECT version FROM posts WHERE id = ?`, c.ID).Scan(&v)
			return schedule(tx, JobCheck, c.ID, v, c.At, c.At)
		}
		var v int64
		tx.QueryRow(`SELECT version FROM comments WHERE id = ?`, c.ID).Scan(&v)
		return schedule(tx, JobCheckComment, c.ID, v, c.At, c.At)
	})
}

// Vote is a member's Keep or Hide on a flagged item. Who may vote is
// checked here (members of 30+ days with some approved content, not the
// author, not someone they're arguing with in that thread), so the rule
// can't be skipped by a crafted request. When hides reach the group's
// threshold and outnumber keeps, the item is hidden; when keeps reach it,
// the flag is cleared for good.
type Vote struct {
	GroupID int64
	Kind    string
	ID      int64
	UserID  int64
	Vote    string // keep | hide
	At      int64
}

// ErrCantVote explains who may vote. (An input error: it's shown to them.)
var ErrCantVote error = &InputError{msg: "votes on flagged posts are for members of at least 30 days who have posted in the group, and not for anyone in the disagreement"}

func (c *Vote) Apply(a *Applier) (any, error) {
	if c.Vote != "keep" && c.Vote != "hide" {
		return nil, Invalid("a vote is keep or hide")
	}
	var result string
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		it, err := loadItem(tx, c.Kind, c.ID)
		if err != nil {
			return err
		}
		if it.status != "flagged" {
			return Invalid("only flagged items get votes")
		}
		if err := canVote(tx, it, c.UserID, c.At); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO flag_votes (kind, item_id, user_id, vote, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (kind, item_id, user_id) DO UPDATE SET vote = excluded.vote`, c.Kind, c.ID, c.UserID, c.Vote, c.At); err != nil {
			return err
		}
		var keeps, hides, threshold int
		tx.QueryRow(`SELECT COALESCE(SUM(vote = 'keep'), 0), COALESCE(SUM(vote = 'hide'), 0) FROM flag_votes
			WHERE kind = ? AND item_id = ?`, c.Kind, c.ID).Scan(&keeps, &hides)
		if err := tx.QueryRow(`SELECT vote_threshold FROM settings WHERE id = 1`).Scan(&threshold); err != nil {
			return err
		}
		switch {
		case hides >= threshold && hides > keeps:
			if _, err = changeStatus(tx, c.GroupID, it, "auto_hidden", c.At); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE `+it.table()+` SET flagged_by = 'vote' WHERE id = ?`, c.ID); err != nil {
				return err
			}
			result = "hidden"
			return modLog(tx, 0, "vote_hide", c.Kind, c.ID, "", c.At)
		case keeps >= threshold:
			if _, err := changeStatus(tx, c.GroupID, it, "visible", c.At); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE `+it.table()+` SET ai_cleared = 1, flagged_by = NULL, flag_category = NULL, flag_reason = NULL
				WHERE id = ?`, c.ID); err != nil {
				return err
			}
			result = "kept"
			return modLog(tx, 0, "vote_keep", c.Kind, c.ID, "", c.At)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// canVote is the voting rule. "Someone they're arguing with" is read
// narrowly and mechanically: for a reply, the author of the comment it
// replies to; for any comment, the authors of replies to it.
func canVote(tx *sql.Tx, it *item, userID, at int64) error {
	var status string
	var since int64
	if err := tx.QueryRow(`SELECT status, created_at FROM memberships WHERE user_id = ?`, userID).Scan(&status, &since); err != nil ||
		status != "active" || since > at-MemberVoteAge || userID == it.author {
		return ErrCantVote
	}
	var approved int
	tx.QueryRow(`SELECT (SELECT COUNT(*) FROM posts WHERE user_id = ?1 AND status IN ('visible', 'flagged'))
		+ (SELECT COUNT(*) FROM comments WHERE user_id = ?1 AND status IN ('visible', 'flagged'))`, userID).Scan(&approved)
	if approved == 0 {
		return ErrCantVote
	}
	if it.kind == "comment" {
		var n int
		tx.QueryRow(`SELECT COUNT(*) FROM comments WHERE user_id = ?1 AND (id = ?2 OR parent_id = ?3)`,
			userID, it.parentID, it.id).Scan(&n)
		if n > 0 {
			return ErrCantVote
		}
	}
	return nil
}

// SetPostFlag locks or unlocks a thread (no new comments), or pins or
// unpins a post (top of the feed).
type SetPostFlag struct {
	GroupID int64
	PostID  int64
	Flag    string // locked | pinned
	On      bool
	By      int64
	At      int64
}

func (c *SetPostFlag) Apply(a *Applier) (any, error) {
	if c.Flag != "locked" && c.Flag != "pinned" {
		return nil, Invalid("unknown flag")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE posts SET `+c.Flag+` = ? WHERE id = ?`, c.On, c.PostID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrGone
		}
		action := map[string]string{"locked": "lock", "pinned": "pin"}[c.Flag]
		if !c.On {
			action = "un" + action
		}
		return modLog(tx, c.By, action, "post", c.PostID, "", c.At)
	})
}

// SetRole makes a member an owner, a mod, or a plain member (the group's
// owners do this). A group always keeps at least one owner.
type SetRole struct {
	GroupID int64
	UserID  int64
	Role    string
	By      int64
	At      int64
}

func (c *SetRole) Apply(a *Applier) (any, error) {
	if c.Role != "owner" && c.Role != "mod" && c.Role != "member" {
		return nil, Invalid("a role is owner, mod or member")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var role, status string
		if err := tx.QueryRow(`SELECT role, status FROM memberships WHERE user_id = ?`, c.UserID).Scan(&role, &status); err != nil || status != "active" {
			return Invalid("only an active member can be given a role")
		}
		if role == "owner" && c.Role != "owner" {
			var owners int
			tx.QueryRow(`SELECT COUNT(*) FROM memberships WHERE role = 'owner' AND status = 'active'`).Scan(&owners)
			if owners <= 1 {
				return Invalid("a group needs at least one owner")
			}
		}
		if _, err := tx.Exec(`UPDATE memberships SET role = ? WHERE user_id = ?`, c.Role, c.UserID); err != nil {
			return err
		}
		return modLog(tx, c.By, "role_"+c.Role, "user", c.UserID, "", c.At)
	})
}

// Ban removes someone from a group and keeps them out, until Until (0 =
// for good). Mods can ban members; only owners (or the operator) can ban
// a mod, and owners can't be banned. Their posts stay up; a ban is about
// the person, and mods remove content separately.
type Ban struct {
	GroupID    int64
	UserID     int64
	Until      int64 // 0 = permanent
	Reason     string
	By         int64
	ByOperator bool
	At         int64
}

func (c *Ban) Apply(a *Applier) (any, error) {
	if c.Until != 0 && c.Until <= c.At {
		return nil, Invalid("a ban has to end in the future")
	}
	if c.UserID == c.By {
		return nil, Invalid("you can't ban yourself")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRow(`SELECT role FROM memberships WHERE user_id = ?`, c.UserID).Scan(&role)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var byRole string
		tx.QueryRow(`SELECT role FROM memberships WHERE user_id = ? AND status = 'active'`, c.By).Scan(&byRole)
		switch {
		case role == "owner":
			return Invalid("an owner can't be banned; make them a member first")
		case role == "mod" && byRole != "owner" && !c.ByOperator:
			return Invalid("only an owner can ban a moderator")
		}
		// Banning someone who isn't a member yet keeps them from joining.
		if _, err := tx.Exec(`INSERT INTO memberships (user_id, role, status, banned_until, created_at) VALUES (?, 'member', 'banned', ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET role = 'member', status = 'banned', banned_until = excluded.banned_until`,
			c.UserID, c.Until, c.At); err != nil {
			return err
		}
		return modLog(tx, c.By, "ban", "user", c.UserID, strings.TrimSpace(c.Reason), c.At)
	})
}

// Unban lifts a ban. They're not a member again until they join.
type Unban struct {
	GroupID int64
	UserID  int64
	By      int64
	At      int64
}

func (c *Unban) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM memberships WHERE user_id = ? AND status = 'banned'`, c.UserID); err != nil {
			return err
		}
		return modLog(tx, c.By, "unban", "user", c.UserID, "", c.At)
	})
}

// SuspendUser is the site operator suspending an account everywhere (0 =
// lift it). Its sessions end at once, on every node.
type SuspendUser struct {
	UserID int64
	Until  int64
	At     int64
}

func (c *SuspendUser) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET suspended_until = ? WHERE id = ? AND is_operator = 0`, c.Until, c.UserID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Invalid("no such account (or it's an operator)")
		}
		if c.Until > c.At {
			_, err = tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, c.UserID)
		}
		return err
	})
}
