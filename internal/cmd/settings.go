package cmd

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// UpdateSettings changes a group's settings (owners, and the operator).
// Set holds only the fields being changed, by column name; each milestone's
// settings page adds its fields to settingRules rather than growing a new
// command.
type UpdateSettings struct {
	GroupID int64
	Set     map[string]any
	By      int64
	At      int64
}

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

// mirrored lists the settings site.db keeps a copy of (see the groups
// table), so a node without this group's file can still list the group and
// apply the sister-group visibility rule.
var mirrored = map[string]bool{"name": true, "visibility": true, "ai_enabled": true}

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
	err := a.Group(c.GroupID, func(tx *sql.Tx) error {
		// A public group going private: its FAQ goes private with it,
		// unless the same change says otherwise. The owner can then turn
		// the public preview on deliberately (plan section 4, "Privacy").
		if vis, ok := values["visibility"]; ok && vis != "public" {
			if _, set := values["public_faq"]; !set {
				var old string
				tx.QueryRow(`SELECT visibility FROM settings WHERE id = 1`).Scan(&old)
				if old == "public" {
					sets = append(sets, "public_faq = 0")
				}
			}
		}
		if _, err := tx.Exec(`UPDATE settings SET `+strings.Join(sets, ", ")+` WHERE id = 1`, args...); err != nil {
			return err
		}
		return modLog(tx, c.By, "settings", "group", c.GroupID, strings.Join(cols, ", "), c.At)
	})
	if err != nil {
		return nil, err
	}
	var siteSets []string
	var siteArgs []any
	for _, k := range cols {
		if mirrored[k] {
			siteSets = append(siteSets, k+" = ?")
			siteArgs = append(siteArgs, values[k])
		}
	}
	if len(siteSets) == 0 {
		return nil, nil
	}
	// A second transaction, on site.db; as with CreateGroup, a replay
	// after a crash between the two finishes whichever part is missing.
	return nil, a.Site(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE groups SET `+strings.Join(siteSets, ", ")+` WHERE id = ?`, append(siteArgs, c.GroupID)...)
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
