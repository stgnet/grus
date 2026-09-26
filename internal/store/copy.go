package store

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Copies of files, and the stable copies the replication engine keeps
// (docs/replication.md). groupID 0 means site.db throughout.

// CopyFile writes a consistent copy of a live file to dst (which must not
// exist). It uses SQLite's VACUUM INTO, which reads inside one
// transaction, so the copy is one point in time even while writes go on,
// and the result is a compact, standalone file (no -wal to carry). A
// group's export uses it.
func (s *Store) CopyFile(groupID int64, dst string) error {
	db := s.Site()
	if groupID != 0 {
		var err error
		if db, err = s.Group(groupID); err != nil {
			return err
		}
	}
	return vacuumInto(db, dst)
}

func vacuumInto(db *sql.DB, path string) error {
	_, err := db.Exec(`VACUUM INTO ?`, path)
	return err
}

// Live returns a live file's handle, creating the file if it doesn't exist:
// site.db, or a group's (the engine's GroupOrCreate).
func (s *Store) Live(groupID int64) (*sql.DB, error) {
	if groupID == 0 {
		return s.Site(), nil
	}
	return s.GroupOrCreate(groupID)
}

// LivePath is the path of a live file's current generation.
func (s *Store) LivePath(groupID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.livePath(groupID)
	return p
}

// SwapLive makes the file at src (a rebuilt copy, in the same directory as
// the live file) the live file, as its next generation. Pages that already
// hold the old handle finish with it; the old file is removed a minute
// later.
func (s *Store) SwapLive(groupID int64, src string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.livePath(groupID)
	if err != nil {
		return err
	}
	next := nextLivePath(cur)
	if err := os.Rename(src, next); err != nil {
		return err
	}
	migrations := groupMigrations
	if groupID == 0 {
		migrations = siteMigrations
	}
	db, err := openDB(next, migrations)
	if err != nil {
		return err
	}
	var old *sql.DB
	if groupID == 0 {
		old, s.site = s.site, db
	} else {
		old = s.groups[groupID]
		s.groups[groupID] = db
	}
	s.live[groupID] = next
	if old != nil {
		closeLater(old, cur)
	} else {
		go func() { removeDB(cur) }()
	}
	s.Touch(groupID)
	return nil
}

// stablePath is where a file's stable copy lives.
func (s *Store) stablePath(groupID int64) string {
	if groupID == 0 {
		return filepath.Join(s.dir, "site.stable.db")
	}
	return filepath.Join(s.fileDir(groupID), "stable.db")
}

// HasStable reports whether a file has a stable copy yet.
func (s *Store) HasStable(groupID int64) bool {
	_, err := os.Stat(s.stablePath(groupID))
	return err == nil
}

// Stable returns a file's stable copy, creating an empty one if there's
// none. Only the replication engine uses it, one log at a time.
func (s *Store) Stable(groupID int64) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.stables[groupID]; db != nil {
		return db, nil
	}
	p := s.stablePath(groupID)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, err
	}
	migrations := groupMigrations
	if groupID == 0 {
		migrations = siteMigrations
	}
	db, err := openDB(p, migrations)
	if err != nil {
		return nil, err
	}
	s.stables[groupID] = db
	return db, nil
}

// InitStable makes a file's first stable copy from its live file. It's
// for a file with no operations yet beyond its base: a new file, or one
// just upgraded from Raft, where the two copies start out the same.
func (s *Store) InitStable(groupID int64) error {
	live, err := s.Live(groupID)
	if err != nil {
		return err
	}
	tmp, err := s.TempPath(groupID)
	if err != nil {
		return err
	}
	if err := vacuumInto(live, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return s.ReplaceStable(groupID, tmp)
}

// ReplaceStable makes the file at src (in the same directory) the stable
// copy. Only the engine holds a handle to it, and it holds the log's lock,
// so the old one can be closed at once.
func (s *Store) ReplaceStable(groupID int64, src string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.stables[groupID]; db != nil {
		db.Close()
		delete(s.stables, groupID)
	}
	p := s.stablePath(groupID)
	if err := removeDB(p); err != nil {
		return err
	}
	return os.Rename(src, p)
}

// CopyStable writes the stable copy's current contents to dst, a new file
// in dir (the same directory as the file, so a later rename is a rename).
// The caller holds the log's lock, so nothing is writing to it: a WAL
// checkpoint then a plain copy of the main file is exact, and much faster
// than VACUUM INTO for a big group.
func (s *Store) CopyStable(groupID int64) (string, error) {
	db, err := s.Stable(groupID)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.fileDir(groupID), "rebuild-*.db")
	if err != nil {
		return "", err
	}
	tmp.Close()
	var busy, logFrames, done int
	err = db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done)
	if err == nil && busy == 0 {
		err = copyFile(s.stablePath(groupID), tmp.Name())
	} else {
		// Couldn't checkpoint fully: fall back to the slow, always-exact way.
		os.Remove(tmp.Name())
		err = vacuumInto(db, tmp.Name())
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// TempPath is a new, unused file name in a file's directory, for a copy
// that will be renamed into place.
func (s *Store) TempPath(groupID int64) (string, error) {
	if err := os.MkdirAll(s.fileDir(groupID), 0o750); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.fileDir(groupID), "incoming-*.db")
	if err != nil {
		return "", err
	}
	f.Close()
	os.Remove(f.Name())
	return f.Name(), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// DropGroup deletes this node's copies of a group, when the group is taken
// off the node. The other nodes' copies are untouched.
func (s *Store) DropGroup(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.groups[id]; db != nil {
		db.Close()
		delete(s.groups, id)
	}
	if db := s.stables[id]; db != nil {
		db.Close()
		delete(s.stables, id)
	}
	delete(s.live, id)
	return os.RemoveAll(s.fileDir(id))
}

// removeDB removes a SQLite file and its WAL files.
func removeDB(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
