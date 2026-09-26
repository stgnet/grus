package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// UpdateSettings changes a group's settings (owners, and the operator).
// Set holds only the fields being changed, by column name; each milestone's
// settings page adds its fields to settingRules rather than growing a new
// command.
//
// Settings live in site.db (group_settings), with the rest of the site's
// configuration, so this is a site-log command. The group's own file keeps
// a copy, which the group's own commands read (a command reads only its
// own file): this sends the whole row there as CopySettings, along with
// the mod log line.
type UpdateSettings struct {
	GroupID int64
	Set     map[string]any
	By      int64
	At      int64
}

func (*UpdateSettings) siteLog() {}

// settingRules lists the settings a page may change, with a check for each
// value. JSON brings numbers in as float64 and bools as bool, so the checks
// normalize to what SQLite stores.
var settingRules = map[string]func(any) (any, error){
	"name":            text(1, 100),
	"description":     text(0, 2000),
	"rules":           text(0, 5000),
	"join_questions":  text(0, 2000),
	"visibility":      oneOf("public", "private", "hidden"),
	"join_policy":     oneOf("open", "approval", "invite"),
	"allow_anonymous": boolean,
	"hold_first_post": boolean,
	"allow_indexing":  boolean,
	"ai_enabled":      boolean,
	"ai_local_only":   boolean,
	"public_faq":      boolean,
	"notify_hidden":   boolean,
	"vote_threshold":  number(1, 50),
}

func text(minLen, maxLen int) func(any) (any, error) {
	return func(v any) (any, error) {
		s, ok := v.(string)
		s = strings.TrimSpace(s)
		if !ok || len(s) < minLen || len(s) > maxLen {
			return nil, fmt.Errorf("needs %d to %d characters", minLen, maxLen)
		}
		return s, nil
	}
}

func oneOf(options ...string) func(any) (any, error) {
	return func(v any) (any, error) {
		s, _ := v.(string)
		for _, o := range options {
			if s == o {
				return s, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(options, ", "))
	}
}

func boolean(v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("must be on or off")
	}
	if b {
		return 1, nil
	}
	return 0, nil
}

func number(lo, hi int) func(any) (any, error) {
	return func(v any) (any, error) {
		var n int
		switch x := v.(type) {
		case int:
			n = x
		case int64:
			n = int(x)
		case float64:
			n = int(x)
		default:
			return nil, fmt.Errorf("must be a number")
		}
		if n < lo || n > hi {
			return nil, fmt.Errorf("must be %d to %d", lo, hi)
		}
		return n, nil
	}
}

func (c *UpdateSettings) Apply(a *Applier) (any, error) {
	// Columns in a fixed order, so every node builds the same statement.
	var cols []string
	for k := range c.Set {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	values := map[string]any{}
	var sets []string
	var args []any
	for _, k := range cols {
		check, ok := settingRules[k]
		if !ok {
			return nil, Invalid("%s isn't a setting", k)
		}
		v, err := check(c.Set[k])
		if err != nil {
			return nil, Invalid("%s %v", strings.ReplaceAll(k, "_", " "), err)
		}
		values[k] = v
		sets = append(sets, k+" = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		return nil, nil
	}
	err := a.Site(func(tx *sql.Tx) error {
		var old string
		err := tx.QueryRow(`SELECT visibility FROM group_settings WHERE group_id = ?`, c.GroupID).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			// A group made before settings moved here, whose row is still
			// on its way up from its own file (ExportSettings). That takes
			// moments after an upgrade; until then, changes wait.
			return Invalid("this group's settings are being moved; please try again in a minute")
		}
		if err != nil {
			return err
		}
		// A public group going private: its FAQ goes private with it,
		// unless the same change says otherwise. The owner can then turn
		// the public preview on deliberately (plan section 4, "Privacy").
		if vis, ok := values["visibility"]; ok && vis != "public" && old == "public" {
			if _, set := values["public_faq"]; !set {
				sets = append(sets, "public_faq = 0")
			}
		}
		if _, err := tx.Exec(`UPDATE group_settings SET `+strings.Join(sets, ", ")+` WHERE group_id = ?`,
			append(args, c.GroupID)...); err != nil {
			return err
		}
		// The group list's own copies of the name, visibility and "Use
		// AI", which the list pages read without a join.
		if _, err := tx.Exec(`UPDATE groups SET (name, visibility, ai_enabled) =
			(SELECT name, visibility, ai_enabled FROM group_settings WHERE group_id = ?1) WHERE id = ?1`, c.GroupID); err != nil {
			return err
		}
		// The group file's copy, sent as the values now are, so a repeat or
		// a late delivery writes the same thing.
		cp := &CopySettings{GroupID: c.GroupID}
		if err := tx.QueryRow(`SELECT `+settingsCols+` FROM group_settings WHERE group_id = ?`, c.GroupID).
			Scan(cp.Row.fields()...); err != nil {
			return err
		}
		if err := send(tx, cp, c.At); err != nil {
			return err
		}
		return send(tx, &ModLogEntry{GroupID: c.GroupID, By: c.By, Action: "settings", TargetType: "group",
			TargetID: c.GroupID, Reason: strings.Join(cols, ", "), At: c.At}, c.At)
	})
	return nil, err
}

// settingsCols are the settings columns, the same in site.db's
// group_settings and in a group file's settings row. SettingsRow.fields
// lists its fields in this order.
const settingsCols = `name, description, rules, visibility, join_policy, join_questions, allow_anonymous,
	hold_first_post, allow_indexing, ai_enabled, ai_local_only, public_faq, vote_threshold, notify_hidden`

// SettingsRow is a whole settings row, as carried between site.db and a
// group's file.
type SettingsRow struct {
	Name, Description, Rules, Visibility, JoinPolicy, JoinQuestions string
	AllowAnonymous, HoldFirstPost, AllowIndexing, AIEnabled         bool
	AILocalOnly, PublicFAQ                                          bool
	VoteThreshold                                                   int
	NotifyHidden                                                    bool
}

// fields are pointers to the fields in settingsCols order, for Scan.
func (r *SettingsRow) fields() []any {
	return []any{&r.Name, &r.Description, &r.Rules, &r.Visibility, &r.JoinPolicy, &r.JoinQuestions, &r.AllowAnonymous,
		&r.HoldFirstPost, &r.AllowIndexing, &r.AIEnabled, &r.AILocalOnly, &r.PublicFAQ, &r.VoteThreshold, &r.NotifyHidden}
}

// values are the fields' values in settingsCols order, for Exec.
func (r *SettingsRow) values() []any {
	return []any{r.Name, r.Description, r.Rules, r.Visibility, r.JoinPolicy, r.JoinQuestions, r.AllowAnonymous,
		r.HoldFirstPost, r.AllowIndexing, r.AIEnabled, r.AILocalOnly, r.PublicFAQ, r.VoteThreshold, r.NotifyHidden}
}

// CopySettings writes a group's settings, as site.db now has them, into
// the group's own file: the copy its own commands read.
type CopySettings struct {
	GroupID int64
	Row     SettingsRow
}

func (c *CopySettings) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO settings (id, `+settingsCols+`)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.Row.values()...)
		return err
	})
}

// ExportSettings copies a group's settings from its own file up to
// site.db, once, for a group made before settings moved there. The
// leader of the group's log submits it when site.db has no row for the
// group; it sends AdoptSettings to the site log.
type ExportSettings struct {
	GroupID int64
	At      int64
}

func (c *ExportSettings) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		ad := &AdoptSettings{GroupID: c.GroupID}
		err := tx.QueryRow(`SELECT ` + settingsCols + ` FROM settings WHERE id = 1`).Scan(ad.Row.fields()...)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // InitGroup hasn't reached this file yet; nothing to copy
		}
		if err != nil {
			return err
		}
		return send(tx, ad, c.At)
	})
}

// AdoptSettings gives site.db a group's settings from the group's file.
// It never overwrites: once site.db has the row, site.db is the truth.
type AdoptSettings struct {
	GroupID int64
	Row     SettingsRow
}

func (*AdoptSettings) siteLog() {}

func (c *AdoptSettings) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO group_settings (group_id, `+settingsCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append([]any{c.GroupID}, c.Row.values()...)...)
		return err
	})
}

// RecordUsage adds a node's AI usage counts since its last report to the
// daily totals. Nodes count in memory and report every few minutes, so a
// search isn't a replicated write.
type RecordUsage struct {
	Day  string // YYYY-MM-DD
	Node string
	Rows []UsageRow
}

// UsageRow is one purpose's counts.
type UsageRow struct {
	Purpose      string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	Seconds      float64
	Failures     int64
}

func (c *RecordUsage) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		for _, r := range c.Rows {
			if _, err := tx.Exec(`INSERT INTO ai_usage (day, node, purpose, calls, input_tokens, output_tokens, seconds, failures)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (day, node, purpose) DO UPDATE SET
				  calls = calls + excluded.calls,
				  input_tokens = input_tokens + excluded.input_tokens,
				  output_tokens = output_tokens + excluded.output_tokens,
				  seconds = seconds + excluded.seconds,
				  failures = failures + excluded.failures`,
				c.Day, c.Node, r.Purpose, r.Calls, r.InputTokens, r.OutputTokens, r.Seconds, r.Failures); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveFeedback keeps a search someone marked "not what I was looking for",
// with the cards they were shown, for tuning. It's the only time a
// question's text is stored, and the link says so.
type SaveFeedback struct {
	GroupID  int64
	ID       int64
	UserID   int64
	Question string
	CitedIDs string
	At       int64
}

func (c *SaveFeedback) Apply(a *Applier) (any, error) {
	if len(c.Question) > 1000 || len(c.CitedIDs) > 1000 {
		return nil, Invalid("too long")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO ai_feedback (id, user_id, question, cited_ids, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.ID, c.UserID, c.Question, c.CitedIDs, c.At)
		return err
	})
}
