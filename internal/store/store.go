// Package store owns the SQLite files: opening them, their schemas, and
// every read query. Writes don't live here; they're commands in
// internal/cmd, applied through the replicated log.
//
// On disk, under the data directory:
//
//	site.db                  identity, domains, the list of groups
//	groups/<id>/group.db     one file per group
//
// Keeping each group in its own file is what lets a group later be moved,
// exported, or replicated to its own set of nodes: it's a directory.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	_ "modernc.org/sqlite" // pure Go SQLite: no cgo, truly static builds
)

// Store holds the open database handles.
type Store struct {
	dir string

	// mu guards the handles, which are swapped out wholesale when a Raft
	// snapshot is restored (Replace). Every accessor takes it briefly.
	mu     sync.Mutex
	site   *sql.DB
	groups map[int64]*sql.DB
}

// Open opens (creating if needed) site.db under dir. Group files are opened
// on first use.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "groups"), 0o750); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, groups: map[int64]*sql.DB{}}
	var err error
	s.site, err = openDB(s.sitePath(), siteMigrations)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the data directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) sitePath() string { return filepath.Join(s.dir, "site.db") }

func (s *Store) groupPath(id int64) string {
	return filepath.Join(s.dir, "groups", strconv.FormatInt(id, 10), "group.db")
}

// Site returns the site database.
func (s *Store) Site() *sql.DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.site
}

// Group returns a group's database, creating the file if it doesn't exist
// yet. Only the CreateGroup command should be creating it; readers check
// the groups table first.
func (s *Store) Group(id int64) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.groups[id]; db != nil {
		return db, nil
	}
	p := s.groupPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, err
	}
	db, err := openDB(p, groupMigrations)
	if err != nil {
		return nil, err
	}
	s.groups[id] = db
	return db, nil
}

// GroupFileIDs lists the groups that have a file on disk, in id order.
// Snapshots and backups use this rather than the groups table, so they copy
// exactly what's on disk.
func (s *Store) GroupFileIDs() ([]int64, error) {
	ents, err := os.ReadDir(filepath.Join(s.dir, "groups"))
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range ents {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || !e.IsDir() {
			continue
		}
		if _, err := os.Stat(s.groupPath(id)); err == nil {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Close closes every handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *Store) closeLocked() error {
	errs := []error{s.site.Close()}
	for id, db := range s.groups {
		errs = append(errs, db.Close())
		delete(s.groups, id)
	}
	return errors.Join(errs...)
}

// openDB opens one SQLite file with the settings every file uses and brings
// its schema up to date.
func openDB(path string, migrations []string) (*sql.DB, error) {
	// WAL: readers never block the writer, and page views are all reads.
	// synchronous=NORMAL: a power cut can lose the last few commits, but
	// that's fine here because the Raft log (fsynced) is the durable record
	// and the `applied` index rolls back with them, so they're re-applied.
	// busy_timeout: the log applier and a backup can briefly contend.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return db, nil
}

func migrate(db *sql.DB, migrations []string) error {
	var have int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return err
	}
	for v := have; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		// PRAGMA can't take a bound parameter; v is our own integer.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// AppliedIndex reads a database's last applied log index.
func AppliedIndex(q interface {
	QueryRow(string, ...any) *sql.Row
}) (uint64, error) {
	var idx uint64
	err := q.QueryRow(`SELECT log_index FROM applied WHERE id = 1`).Scan(&idx)
	return idx, err
}

// MaxApplied is the highest applied index across every file. The local
// (non-Raft) log starts counting after it.
func (s *Store) MaxApplied() (uint64, error) {
	max, err := AppliedIndex(s.Site())
	if err != nil {
		return 0, err
	}
	ids, err := s.GroupFileIDs()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		db, err := s.Group(id)
		if err != nil {
			return 0, err
		}
		idx, err := AppliedIndex(db)
		if err != nil {
			return 0, err
		}
		if idx > max {
			max = idx
		}
	}
	return max, nil
}
