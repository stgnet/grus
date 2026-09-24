package cmd

import (
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Logs (plan section 8, "Who holds what"): from M7 there isn't one
// replicated log but one per file. site.db has its own log, which every
// node follows, and each group's file has its own, followed only by the
// nodes that hold that group. That's what lets a small VPS hold a few
// groups and not the whole network.
//
// So every command belongs to exactly one log, and writes exactly one
// file: the one that log is for. It also reads only that file. Reading a
// second file would make the result depend on how far that other file's
// log had got on this node, which differs from node to node, and the
// copies would drift apart. (Anything a command needs from elsewhere, the
// submitter looks up and puts in the command, like a timestamp.)
//
// A change that has to reach another file (ending a sister pairing takes
// links down in both groups; changing a group's name updates the site's
// copy) is sent through that file's outbox: the command writes the
// follow-up command into an outbox table in its own transaction, and the
// leader of the log relays it to its target log afterwards (see Relay in
// internal/cluster). Written in the same transaction, the follow-up can't
// be lost; relayed "at least once", it must be safe to apply twice.

// LogID names a log: SiteLog, or a group's id.
type LogID int64

// SiteLog is site.db's log.
const SiteLog LogID = 0

func (l LogID) String() string {
	if l == SiteLog {
		return "site"
	}
	return "g" + strconv.FormatInt(int64(l), 10)
}

// ParseLogID reads a LogID's String form back.
func ParseLogID(s string) (LogID, error) {
	if s == "site" {
		return SiteLog, nil
	}
	if n, err := strconv.ParseInt(strings.TrimPrefix(s, "g"), 10, 64); err == nil && strings.HasPrefix(s, "g") && n > 0 {
		return LogID(n), nil
	}
	return 0, fmt.Errorf("bad log name %q", s)
}

// siteCommand marks a command that has a GroupID field but belongs to the
// site log (it changes the group list, or a pairing, which live there).
type siteCommand interface{ siteLog() }

// LogOf says which log a command goes to: its group's, when it has a
// non-zero GroupID field, otherwise the site's (or when it's marked as a
// site command).
func LogOf(c Command) LogID {
	if _, ok := c.(siteCommand); ok {
		return SiteLog
	}
	v := reflect.ValueOf(c)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if f := v.FieldByName("GroupID"); f.IsValid() && f.Kind() == reflect.Int64 && f.Int() > 0 {
		return LogID(f.Int())
	}
	return SiteLog
}

// send puts a follow-up command in this file's outbox, to be applied to
// target's log after this command commits. at orders nothing; it's kept
// for looking at a stuck outbox.
func send(tx *sql.Tx, c Command, at int64) error {
	data, err := Encode(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO outbox (target, command, created_at) VALUES (?, ?, ?)`, int64(LogOf(c)), data, at)
	return err
}

// OutboxDone removes relayed commands from a log's outbox, up to UpTo.
// GroupID 0 is site.db's outbox.
type OutboxDone struct {
	GroupID int64
	UpTo    int64
}

func (c *OutboxDone) Apply(a *Applier) (any, error) {
	del := func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM outbox WHERE id <= ?`, c.UpTo)
		return err
	}
	if c.GroupID == 0 {
		return nil, a.Site(del)
	}
	return nil, a.Group(c.GroupID, del)
}

// ModLogEntry adds a line to a group's mod log, for actions decided in
// site.db (sister pairings) that the group's mods should still see.
type ModLogEntry struct {
	GroupID    int64
	By         int64
	Action     string
	TargetType string
	TargetID   int64
	Reason     string
	At         int64
}

func (c *ModLogEntry) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		return modLog(tx, c.By, c.Action, c.TargetType, c.TargetID, c.Reason, c.At)
	})
}
