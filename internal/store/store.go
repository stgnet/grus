// Package store owns the SQLite files: opening them, their schemas, and
// every read query. Writes don't live here; they're commands in
// internal/cmd, applied through the replicated log.
//
// On disk, under the data directory:
//
//	site.db                  identity, domains, the list of groups
//	groups/<id>/group.db     one file per group
//
// Each of those is the file's **live** copy, which pages read. Beside each
// is its **stable** copy (site.stable.db, groups/<id>/stable.db), which
// only the replication engine uses: it holds the file as of its stable
// point, and a rewind starts from it (docs/replication.md). A rewind makes
// a new live file and switches to it, so the live file's name carries a
// generation number after the first one (site.3.db); the highest on disk
// is the current one.
//
// Keeping each group in its own directory is what lets a group be moved,
// exported, or replicated to its own set of nodes.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite: no cgo, truly static builds
)

// Store holds the open database handles.
type Store struct {
	dir string

	// mu guards the handles and paths, which change when a live file is
	// swapped for a rebuilt one (SwapLive). Every accessor takes it
	// briefly.
	mu      sync.Mutex
	site    *sql.DB
	groups  map[int64]*sql.DB
	live    map[int64]string  // current live path, by file (0 = site.db)
	stables map[int64]*sql.DB // stable copies, opened by the engine

	// versions counts changes to each live file (by file, 0 = site.db),
	// so the page cache can tell whether a cached page is still current.
	vmu      sync.Mutex
	versions map[int64]*atomic.Int64
}

// Open opens (creating if needed) site.db under dir. Group files are opened
// on first use.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "groups"), 0o750); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, groups: map[int64]*sql.DB{}, live: map[int64]string{},
		stables: map[int64]*sql.DB{}, versions: map[int64]*atomic.Int64{}}
	path, err := s.livePath(0)
	if err != nil {
		return nil, err
	}
	s.site, err = openDB(path, siteMigrations)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the data directory.
func (s *Store) Dir() string { return s.dir }

// fileDir and fileBase say where a file's copies live: site.db in the data
// directory, a group's in groups/<id>/ as group.db.
func (s *Store) fileDir(id int64) string {
	if id == 0 {
		return s.dir
	}
	return filepath.Join(s.dir, "groups", strconv.FormatInt(id, 10))
}

func fileBase(id int64) string {
	if id == 0 {
		return "site"
	}
	return "group"
}

// livePath is the current live file's path. It's found on disk the first
// time (the highest generation), and older generations left behind by a
// crash mid-swap are removed then.
func (s *Store) livePath(id int64) (string, error) {
	if p, ok := s.live[id]; ok {
		return p, nil
	}
	dir, base := s.fileDir(id), fileBase(id)
	re := regexp.MustCompile(`^` + base + `(?:\.(\d+))?\.db$`)
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	best, bestGen := "", -1
	var all []string
	for _, e := range ents {
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		gen := 0
		if m[1] != "" {
			gen, _ = strconv.Atoi(m[1])
		}
		all = append(all, e.Name())
		if gen > bestGen {
			best, bestGen = e.Name(), gen
		}
	}
	for _, name := range all {
		if name != best {
			removeDB(filepath.Join(dir, name))
		}
	}
	if best == "" {
		best = base + ".db"
	}
	p := filepath.Join(dir, best)
	s.live[id] = p
	return p, nil
}

// nextLivePath is the path for the live file's next generation.
func nextLivePath(current string) string {
	dir, name := filepath.Split(current)
	m := regexp.MustCompile(`^(site|group)(?:\.(\d+))?\.db$`).FindStringSubmatch(name)
	gen := 0
	if m != nil && m[2] != "" {
		gen, _ = strconv.Atoi(m[2])
	}
	return filepath.Join(dir, fmt.Sprintf("%s.%d.db", m[1], gen+1))
}

// hasLive reports whether a group's live file exists.
func (s *Store) hasLive(id int64) bool {
	p, err := s.livePath(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Site returns the site database.
func (s *Store) Site() *sql.DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.site
}

// ErrNoGroupFile means this node doesn't hold the group: its file isn't
// here (M7: a node holds only the groups placed on it).
var ErrNoGroupFile = errors.New("this node doesn't hold that group")

// Group returns a group's database for reading, or ErrNoGroupFile when this
// node has no copy of it. It never creates the file: a page that asks about
// a group this node doesn't hold must not leave an empty file behind, which
// would look like a real (empty) copy.
func (s *Store) Group(id int64) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.groups[id]; db != nil {
		return db, nil
	}
	if !s.hasLive(id) && id != RootGroupID {
		return nil, ErrNoGroupFile
	}
	// (Every node holds the root FAQ's file, so it may be created here:
	// an empty one is its true state until an operator writes to it.)
	return s.openGroupLocked(id)
}

// RootGroupID is the group file behind the root FAQ (cmd.RootGroupID has
// the story); every node holds it.
const RootGroupID = 1

// GroupOrCreate is Group for the log applier: it creates the file when a
// group's log writes to it for the first time.
func (s *Store) GroupOrCreate(id int64) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.groups[id]; db != nil {
		return db, nil
	}
	return s.openGroupLocked(id)
}

func (s *Store) openGroupLocked(id int64) (*sql.DB, error) {
	p, err := s.livePath(id)
	if err != nil {
		return nil, err
	}
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
		s.mu.Lock()
		ok := s.hasLive(id)
		s.mu.Unlock()
		if ok {
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
	for id, db := range s.stables {
		errs = append(errs, db.Close())
		delete(s.stables, id)
	}
	return errors.Join(errs...)
}

// openDB opens one SQLite file with the settings every file uses and brings
// its schema up to date.
func openDB(path string, migrations []string) (*sql.DB, error) {
	// WAL: readers never block the writer, and page views are all reads.
	// synchronous=FULL: a write is on disk when its commit returns. The
	// file is the only durable record of an operation (there's no separate
	// log), and a page says "done" only after the commit.
	// busy_timeout: the engine and a page's read can briefly contend.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
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

// OutboxItem is a follow-up command waiting in a file's outbox.
type OutboxItem struct {
	ID      int64
	Target  int64 // 0 = site.db's log, else a group's
	Command []byte
}

// Outbox lists the oldest waiting follow-ups in site.db's outbox (groupID
// 0) or a group's.
func (s *Store) Outbox(groupID int64, limit int) ([]OutboxItem, error) {
	db := s.Site()
	if groupID != 0 {
		var err error
		if db, err = s.Group(groupID); err != nil {
			return nil, err
		}
	}
	rows, err := db.Query(`SELECT id, target, command FROM outbox ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var o OutboxItem
		if err := rows.Scan(&o.ID, &o.Target, &o.Command); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// HasGroup reports whether this node has a file for the group (without
// creating one, which Group would).
func (s *Store) HasGroup(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups[id] != nil {
		return true
	}
	return s.hasLive(id)
}

// FileApplied is the last index applied to site.db (groupID 0) or a group's
// file by the log that ran it: cmd.Direct's count, for tests and tools. 0
// for a group file that doesn't exist.
func (s *Store) FileApplied(groupID int64) (uint64, error) {
	if groupID == 0 {
		return AppliedIndex(s.Site())
	}
	if !s.HasGroup(groupID) {
		return 0, nil
	}
	db, err := s.Group(groupID)
	if err != nil {
		return 0, err
	}
	return AppliedIndex(db)
}

func (s *Store) version(id int64) *atomic.Int64 {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	v := s.versions[id]
	if v == nil {
		v = &atomic.Int64{}
		s.versions[id] = v
	}
	return v
}

// Touch records that a live file changed (0 = site.db). Every commit to a
// live file and every swap calls it.
func (s *Store) Touch(id int64) { s.version(id).Add(1) }

// Version is a count of changes to a live file since this process started:
// if it's the same as before, so is the file.
func (s *Store) Version(id int64) int64 { return s.version(id).Load() }

// closeLater closes a replaced handle after readers have had time to finish
// with it, then removes its files if path is set. A page that took the old
// handle a moment before a swap finishes reading the old copy, which is
// what it would have seen anyway.
func closeLater(db *sql.DB, path string) {
	time.AfterFunc(time.Minute, func() {
		db.Close()
		if path != "" {
			removeDB(path)
		}
	})
}
