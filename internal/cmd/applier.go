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
	"errors"
	"fmt"

	"github.com/stgnet/grus/internal/store"
)

// Command is one replicated write.
type Command interface {
	// Apply makes the command's changes through a, returning an optional
	// value for the submitter (such as the id of a row it chose).
	Apply(a *Applier) (any, error)
}

// Applier gives a command a transaction on the one file its log is for,
// and records what it applied.
//
// Which file: a command on the site log may only use Site, and one on a
// group's log only that group's file (see logs.go for why). Anything else
// is a bug in the command, reported as an error rather than silently
// making copies that differ.
//
// Two ways to run, one per caller:
//
//   - The replication engine (internal/cluster) sets Op: the operation being
//     applied. Its row is written in the command's own transaction, so a
//     file never has an operation without its effects or the other way
//     round, and one already there is skipped (a repeat delivery). DB, if
//     set, is the copy to apply to: the live file (nil), the stable copy,
//     or a rebuild.
//   - Direct (tests and one-shot tools) sets Index instead, a count per
//     file kept in its `applied` table, and an index at or below it is
//     skipped.
//
// (So a command gets one transaction: a second on the same file would be
// skipped as already applied.)
type Applier struct {
	Store *store.Store
	Log   LogID   // the log the command came from
	Index uint64  // Direct: its index in that log
	Op    *Op     // the engine: the operation being applied
	DB    *sql.DB // the engine: the copy to apply to; nil = the live file
	// Fresh is true when the command is being applied for the first time,
	// on the node where it's made: the one application whose outcome
	// decides whether it happens at all. Anywhere else, and on any later
	// replay, the command is known to have succeeded where it was made.
	// Almost every command behaves the same either way; see freeName for
	// the exception.
	Fresh bool

	ran     bool // a transaction ran, so the operation is recorded
	skipped bool // a follow-up whose cause had already been applied
}

// Op is one operation: a command, who made it and where it goes in the
// order (docs/replication.md).
type Op struct {
	Origin  string // node id and incarnation, "n1@1767225600000"
	Seq     int64  // the origin's count of operations on this file
	Stamp   int64  // hybrid logical clock; orders operations
	Cause   string // for a follow-up: the sending operation's identity
	Command []byte // cmd.Encode
}

// Less orders operations: by stamp, then origin, then seq. Every node
// computes the same order from the operations alone.
func (o *Op) Less(p *Op) bool {
	if o.Stamp != p.Stamp {
		return o.Stamp < p.Stamp
	}
	if o.Origin != p.Origin {
		return o.Origin < p.Origin
	}
	return o.Seq < p.Seq
}

// ID is the operation's identity, "origin/seq".
func (o *Op) ID() string { return fmt.Sprintf("%s/%d", o.Origin, o.Seq) }

// Site runs fn in a transaction on site.db.
func (a *Applier) Site(fn func(tx *sql.Tx) error) error {
	if a.Log != SiteLog {
		return fmt.Errorf("a command on log %s tried to write site.db", a.Log)
	}
	db := a.DB
	if db == nil {
		db = a.Store.Site()
	}
	return a.inTx(db, fn)
}

// Group runs fn in a transaction on a group's file, creating the file if
// it doesn't exist yet.
func (a *Applier) Group(groupID int64, fn func(tx *sql.Tx) error) error {
	if a.Log != LogID(groupID) {
		return fmt.Errorf("a command on log %s tried to write group %d's file", a.Log, groupID)
	}
	db := a.DB
	if db == nil {
		var err error
		if db, err = a.Store.GroupOrCreate(groupID); err != nil {
			return err
		}
	}
	return a.inTx(db, fn)
}

func (a *Applier) inTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	if a.Op != nil {
		if err := a.opTx(tx, fn); err != nil {
			return err
		}
	} else {
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
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.ran = true
	if a.DB == nil {
		a.Store.Touch(int64(a.Log))
	}
	return nil
}

// opTx is the engine's half of inTx: skip a follow-up already applied, run
// the command, label what it sent on with its identity, and record it.
func (a *Applier) opTx(tx *sql.Tx, fn func(tx *sql.Tx) error) error {
	if a.Op.Cause != "" {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM causes WHERE cause = ?`, a.Op.Cause).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			a.skipped = true
		} else if _, err := tx.Exec(`INSERT INTO causes (cause) VALUES (?)`, a.Op.Cause); err != nil {
			return err
		}
	}
	if !a.skipped {
		if err := fn(tx); err != nil {
			return err
		}
		// Follow-ups this command just sent get its identity (plus their
		// row, to tell several apart) as their cause. Rows already there
		// have one. Row numbers here are the same on every node for the
		// file's stable copy, which is the only one follow-ups are made
		// from.
		if _, err := tx.Exec(`UPDATE outbox SET cause = ? || '#' || id WHERE cause = ''`, a.Op.ID()); err != nil {
			return err
		}
	}
	return recordOp(tx, a.Op, "", nil)
}

// recordOp writes an operation's row, moves the file's version vector and
// position on, and for a command that failed in its place records why.
func recordOp(tx *sql.Tx, op *Op, name string, failed error) error {
	if _, err := tx.Exec(`INSERT INTO ops (origin, seq, stamp, cause, command) VALUES (?, ?, ?, ?, ?)`,
		op.Origin, op.Seq, op.Stamp, op.Cause, op.Command); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO seqs (origin, seq, stamp) VALUES (?, ?, ?)
		ON CONFLICT (origin) DO UPDATE SET seq = MAX(seq, excluded.seq),
		  stamp = CASE WHEN excluded.seq > seq THEN excluded.stamp ELSE stamp END`, op.Origin, op.Seq, op.Stamp); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE position SET stamp = ?, origin = ?, seq = ? WHERE id = 1`, op.Stamp, op.Origin, op.Seq); err != nil {
		return err
	}
	if failed != nil {
		_, err := tx.Exec(`INSERT OR REPLACE INTO conflicts (origin, seq, stamp, command, error) VALUES (?, ?, ?, ?, ?)`,
			op.Origin, op.Seq, op.Stamp, name, failed.Error())
		return err
	}
	return nil
}

// ApplyOp applies one operation to db, a copy of log's file (see
// Applier.DB), and records it there.
//
// Fresh (the node where it's being made): a command that fails is not an
// operation at all; nothing is recorded and the error goes to the person.
// Anywhere else a failure is recorded as a conflict, the same on every
// node, and the error returned for the log. The exception is a fault in
// the database itself (disk full, I/O), which says nothing about the
// command: it's returned with nothing recorded, and ErrRetry says the
// caller should try again rather than move on.
func ApplyOp(st *store.Store, log LogID, db *sql.DB, op *Op, fresh bool) (any, error) {
	c, err := Decode(op.Command)
	if err != nil {
		return nil, fmt.Errorf("%w: %v (is this node running an older grus?)", ErrRetry, err)
	}
	if LogOf(c) != log {
		err = fmt.Errorf("%s belongs to log %s, not %s", nameOf(c), LogOf(c), log)
	}
	a := &Applier{Store: st, Log: log, DB: db, Op: op, Fresh: fresh}
	var v any
	if err == nil {
		v, err = c.Apply(a)
	}
	if err != nil {
		if isFault(err) {
			return nil, fmt.Errorf("%w: %s: %v", ErrRetry, nameOf(c), err)
		}
		if fresh {
			return nil, fmt.Errorf("%s: %w", nameOf(c), err)
		}
		if rerr := recordAlone(st, log, db, op, nameOf(c), err); rerr != nil {
			return nil, fmt.Errorf("%w: %v", ErrRetry, rerr)
		}
		return nil, fmt.Errorf("%s: %w", nameOf(c), err)
	}
	if !a.ran {
		// A command that wrote nothing (one that's kept only so old
		// operations still decode): its operation is recorded all the same.
		if err := recordAlone(st, log, db, op, "", nil); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRetry, err)
		}
	}
	return v, nil
}

// recordAlone records an operation in a transaction of its own.
func recordAlone(st *store.Store, log LogID, db *sql.DB, op *Op, name string, failed error) error {
	if db == nil {
		var err error
		if db, err = st.Live(int64(log)); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordOp(tx, op, name, failed); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrRetry marks a failure to apply an operation that isn't the command's
// doing: the operation isn't recorded, and should be applied again later.
var ErrRetry = errors.New("couldn't apply the operation")

// isFault reports whether err came from the database itself failing (the
// disk, memory, a lock) rather than from what the command did. A command
// breaking a constraint is the command's doing: that fails the same way on
// every node.
func isFault(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone)
	}
	switch coded.Code() & 0xff { // the primary result code
	case 5, 6, 7, 10, 11, 13, 14, 26: // BUSY, LOCKED, NOMEM, IOERR, CORRUPT, FULL, CANTOPEN, NOTADB
		return true
	}
	return false
}

// Run applies c as entry index of log. The log implementations call this.
func Run(st *store.Store, log LogID, index uint64, c Command) (any, error) {
	if LogOf(c) != log {
		return nil, fmt.Errorf("%s belongs to log %s, not %s", nameOf(c), LogOf(c), log)
	}
	v, err := c.Apply(&Applier{Store: st, Log: log, Index: index, Fresh: true})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", nameOf(c), err)
	}
	return v, nil
}
