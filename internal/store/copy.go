package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// CopyTo writes a consistent copy of every database into dst, laid out the
// same way as the data directory (site.db, groups/<id>/group.db). dst must
// not already hold those files.
//
// It uses SQLite's VACUUM INTO, which reads inside one transaction, so each
// copy is a consistent point in time even while the log keeps applying
// writes, and the result is a compact, standalone file (no -wal to carry).
// Raft snapshots and `grus backup` both use it.
//
// The files are copied one after another, so they're each consistent but
// not from the same instant. That's fine: each file carries its own
// `applied` index, and replaying the log skips whatever a file already has.
func (s *Store) CopyTo(dst string) error {
	if err := os.MkdirAll(filepath.Join(dst, "groups"), 0o750); err != nil {
		return err
	}
	if err := vacuumInto(s.Site(), filepath.Join(dst, "site.db")); err != nil {
		return fmt.Errorf("site.db: %w", err)
	}
	ids, err := s.GroupFileIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		db, err := s.Group(id)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, "groups", strconv.FormatInt(id, 10), "group.db")
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return err
		}
		if err := vacuumInto(db, out); err != nil {
			return fmt.Errorf("group %d: %w", id, err)
		}
	}
	return nil
}

func vacuumInto(db *sql.DB, path string) error {
	_, err := db.Exec(`VACUUM INTO ?`, path)
	return err
}

// Replace swaps every database for the copies in src (a directory in the
// CopyTo layout), used when a Raft snapshot is installed on a node that has
// fallen too far behind to catch up from the log. src must be on the same
// filesystem as the data directory so the moves are renames.
//
// Readers still holding an old handle get an error for that one query;
// accessors hand out the new handles from then on. Restores are rare
// (a new or long-offline node), so that's an acceptable trade for not
// wrapping every query in a lock.
func (s *Store) Replace(src string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.closeLocked(); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(s.sitePath() + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(s.dir, "groups")); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(src, "site.db"), s.sitePath()); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(src, "groups"), filepath.Join(s.dir, "groups")); err != nil {
		return err
	}
	var err error
	s.site, err = openDB(s.sitePath(), siteMigrations)
	return err
}
