package store

import (
	"database/sql"
	"errors"
)

// Reads of the replication tables (the ops, seqs and position tables every
// file has; see replicationTables in schema.go). The engine in
// internal/cluster uses them on any copy of a file: the live one, the
// stable one, or a rebuild. docs/replication.md has the design.

// OpRow is one stored operation.
type OpRow struct {
	Origin  string
	Seq     int64
	Stamp   int64
	Cause   string
	Command []byte
}

// SeqInfo is one origin's entry in a file's version vector: the highest
// seq applied from it, and that operation's stamp.
type SeqInfo struct {
	Seq   int64
	Stamp int64
}

// Position is the last operation applied to a copy, and the Raft index
// the file had when it was upgraded (its base; a fresh file's is 0).
type Position struct {
	Stamp  int64
	Origin string
	Seq    int64
	Base   int64
}

// Querier is a *sql.DB or *sql.Tx.
type Querier interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// Seqs reads a copy's version vector.
func Seqs(q Querier) (map[string]SeqInfo, error) {
	rows, err := q.Query(`SELECT origin, seq, stamp FROM seqs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SeqInfo{}
	for rows.Next() {
		var o string
		var si SeqInfo
		if err := rows.Scan(&o, &si.Seq, &si.Stamp); err != nil {
			return nil, err
		}
		out[o] = si
	}
	return out, rows.Err()
}

// PositionOf reads a copy's position.
func PositionOf(q Querier) (Position, error) {
	var p Position
	err := q.QueryRow(`SELECT stamp, origin, seq, raft_index FROM position WHERE id = 1`).Scan(&p.Stamp, &p.Origin, &p.Seq, &p.Base)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// SetBase records the base of a copy (see Position).
func SetBase(db *sql.DB, base int64) error {
	_, err := db.Exec(`UPDATE position SET raft_index = ? WHERE id = 1`, base)
	return err
}

func scanOps(rows *sql.Rows, err error) ([]OpRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpRow
	for rows.Next() {
		var o OpRow
		if err := rows.Scan(&o.Origin, &o.Seq, &o.Stamp, &o.Cause, &o.Command); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

const opCols = `origin, seq, stamp, cause, command`

// OpsAfter lists a copy's operations after p in order (stamp, origin,
// seq), stopping at stamp upTo when it's > 0.
func OpsAfter(q Querier, p Position, upTo int64) ([]OpRow, error) {
	query := `SELECT ` + opCols + ` FROM ops WHERE (stamp, origin, seq) > (?, ?, ?)`
	args := []any{p.Stamp, p.Origin, p.Seq}
	if upTo > 0 {
		query += ` AND stamp <= ?`
		args = append(args, upTo)
	}
	return scanOps(q.Query(query+` ORDER BY stamp, origin, seq`, args...))
}

// OpsNewerThan lists up to limit of a copy's operations that someone whose
// version vector is have hasn't got, oldest first for each origin, so they
// can be applied as they come. gap reports that some they need have been
// deleted here: they need a fresh copy of the file instead.
func OpsNewerThan(q Querier, have map[string]int64, limit int) (ops []OpRow, gap bool, err error) {
	mine, err := Seqs(q)
	if err != nil {
		return nil, false, err
	}
	for origin, si := range mine {
		if si.Seq <= have[origin] {
			continue
		}
		var first sql.NullInt64
		if err := q.QueryRow(`SELECT MIN(seq) FROM ops WHERE origin = ? AND seq > ?`, origin, have[origin]).Scan(&first); err != nil {
			return nil, false, err
		}
		if !first.Valid || first.Int64 != have[origin]+1 {
			return nil, true, nil
		}
		more, err := scanOps(q.Query(`SELECT `+opCols+` FROM ops WHERE origin = ? AND seq > ? ORDER BY seq LIMIT ?`,
			origin, have[origin], limit-len(ops)))
		if err != nil {
			return nil, false, err
		}
		ops = append(ops, more...)
		if len(ops) >= limit {
			break
		}
	}
	return ops, false, nil
}

// PruneOps deletes a copy's operations that every node that needs them
// has: for each origin, those up to seq upto[origin].
func PruneOps(db *sql.DB, upto map[string]int64) error {
	for origin, seq := range upto {
		if _, err := db.Exec(`DELETE FROM ops WHERE origin = ? AND seq <= ?`, origin, seq); err != nil {
			return err
		}
	}
	return nil
}

// OpenCopy opens a copy of a file (a rebuild, or one fetched from another
// node) with the settings every file uses.
func OpenCopy(path string, groupID int64) (*sql.DB, error) {
	if groupID == 0 {
		return openDB(path, siteMigrations)
	}
	return openDB(path, groupMigrations)
}

// FollowUp is a follow-up waiting in a copy's outbox (see cmd's send).
type FollowUp struct {
	Cause   string
	Command []byte
}

// FollowUps lists the oldest follow-ups waiting in a copy's outbox that
// have been labeled with the operation that sent them.
func FollowUps(db *sql.DB, limit int) ([]FollowUp, error) {
	rows, err := db.Query(`SELECT cause, command FROM outbox WHERE cause != '' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FollowUp
	for rows.Next() {
		var f FollowUp
		if err := rows.Scan(&f.Cause, &f.Command); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FollowUpDone removes a sent follow-up from a copy's outbox.
func FollowUpDone(db *sql.DB, cause string) error {
	_, err := db.Exec(`DELETE FROM outbox WHERE cause = ?`, cause)
	return err
}
