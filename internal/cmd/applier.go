// Package cmd holds every write the system makes, each as a command struct
// with an Apply method. It is the only code that writes SQL.
//
// Why commands and not SQL in handlers: every write is replicated through
// the Raft log and applied by every node to its own copy of the database, in
// the same order. For the copies to stay identical, applying a command must
// be deterministic, so:
//
//   - A command carries every value it writes: ids, timestamps, hashes. The
//     node that accepts the request fills them in before submitting it.
//     Nothing like time.Now() or rand is ever called inside Apply.
//   - Apply may read the database (e.g. "does this email have an account?"),
//     because every node's database is in the same state at that point in
//     the log, so every node gets the same answer.
//   - Apply may fail (a handle already taken). It fails the same way on every
//     node, and the error is returned to whoever submitted the command.
package cmd

import (
	"database/sql"
	"fmt"

	"github.com/stgnet/grus/internal/store"
)

// Command is one replicated write.
type Command interface {
	// Apply makes the command's changes through a, returning an optional
	// value for the submitter (such as the id of a row it chose).
	Apply(a *Applier) (any, error)
}

// Applier gives a command transactions on the databases it writes, and makes
// applying idempotent.
//
// Why idempotent: the SQLite files persist across restarts, but the Raft log
// may hand a node entries it applied before a crash (its record of "applied
// up to" lives in memory). So each file stores the index of the last log
// entry applied to it, updated in the same transaction as the change. An
// entry at or below that index has already been applied to that file and is
// skipped. Because the index is per file, a command that writes two files
// (CreateGroup writes site.db and the new group's file) is completed
// correctly even if the node stopped between the two.
//
// One consequence to keep in mind when writing a command that reads one
// file while writing another: during a replay after a restart, the file it
// reads may already be ahead (later entries applied). Only read what a later
// entry can't change in a way that matters, like "which groups exist".
type Applier struct {
	Store *store.Store
	Index uint64 // the log index of the command being applied
}

// Site runs fn in a transaction on site.db.
func (a *Applier) Site(fn func(tx *sql.Tx) error) error {
	return a.inTx(a.Store.Site(), fn)
}

// Group runs fn in a transaction on a group's file, creating the file if
// it doesn't exist yet.
func (a *Applier) Group(groupID int64, fn func(tx *sql.Tx) error) error {
	db, err := a.Store.Group(groupID)
	if err != nil {
		return err
	}
	return a.inTx(db, fn)
}

func (a *Applier) inTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	done, err := store.AppliedIndex(tx)
	if err != nil {
		return err
	}
	if a.Index <= done {
		return nil // already applied to this file before a restart
	}
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE applied SET log_index = ? WHERE id = 1`, a.Index); err != nil {
		return err
	}
	return tx.Commit()
}

// Run applies c as log entry index. The log implementations call this.
func Run(st *store.Store, index uint64, c Command) (any, error) {
	v, err := c.Apply(&Applier{Store: st, Index: index})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", nameOf(c), err)
	}
	return v, nil
}
