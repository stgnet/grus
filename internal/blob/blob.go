// Package blob stores photos as content-addressed files:
//
//	blobs/ab/cd/<sha256>.jpg     full size (at most 2048px)
//	blobs/ab/cd/<sha256>_t.jpg   thumbnail (about 480px)
//
// The name is the SHA-256 of the full-size file, so a given name's bytes
// never change. That makes copying photos between nodes trivial (copy what's
// missing; nothing is ever updated in place) and keeps them out of the
// databases and the replicated log, which stay small.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store is one node's blob directory.
type Store struct {
	dir string
}

// Open returns the blob store under dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Hash is the name a full-size image's bytes get.
func Hash(full []byte) string {
	sum := sha256.Sum256(full)
	return hex.EncodeToString(sum[:])
}

// Valid reports whether h looks like a blob name, so a request path can't
// point anywhere else on disk.
func Valid(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Path is where a blob lives on disk.
func (s *Store) Path(hash string, thumb bool) string {
	name := hash + ".jpg"
	if thumb {
		name = hash + "_t.jpg"
	}
	return filepath.Join(s.dir, hash[:2], hash[2:4], name)
}

// Has reports whether both sizes of a blob are here.
func (s *Store) Has(hash string) bool {
	for _, t := range []bool{false, true} {
		if _, err := os.Stat(s.Path(hash, t)); err != nil {
			return false
		}
	}
	return true
}

// Put stores both sizes of an image and returns its name.
func (s *Store) Put(full, thumb []byte) (string, error) {
	h := Hash(full)
	if err := s.Write(h, false, full); err != nil {
		return "", err
	}
	return h, s.Write(h, true, thumb)
}

// Write stores one size of a blob, as received from another node. The full
// size is checked against its name, so a bad copy can't take a blob's place.
// Writing goes to a temp file then renames it, so a reader never sees half a
// file.
func (s *Store) Write(hash string, thumb bool, data []byte) error {
	if !Valid(hash) {
		return fmt.Errorf("blob: bad name %q", hash)
	}
	if !thumb && Hash(data) != hash {
		return fmt.Errorf("blob: content doesn't match name %s", hash)
	}
	p := s.Path(hash, thumb)
	if _, err := os.Stat(p); err == nil {
		return nil // already here; same name means same bytes
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Open opens one size of a blob for reading.
func (s *Store) Open(hash string, thumb bool) (*os.File, error) {
	if !Valid(hash) {
		return nil, fs.ErrNotExist
	}
	return os.Open(s.Path(hash, thumb))
}

// Read returns one size of a blob's bytes.
func (s *Store) Read(hash string, thumb bool) ([]byte, error) {
	f, err := s.Open(hash, thumb)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// GC deletes blobs that nothing references any more (keep reports whether a
// name is still used). Files newer than minAge are left alone: a photo is
// stored before the post that uses it is written, and GC mustn't delete it in
// between. It returns how many blobs it removed.
func (s *Store) GC(keep func(hash string) bool, minAge time.Duration) (int, error) {
	n := 0
	cutoff := time.Now().Add(-minAge)
	err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-") {
			// A write that never finished (the process died mid-copy).
			if fi, err := d.Info(); err == nil && fi.ModTime().Before(cutoff) {
				os.Remove(p)
			}
			return nil
		}
		hash := strings.TrimSuffix(strings.TrimSuffix(name, ".jpg"), "_t")
		if !Valid(hash) || keep(hash) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(p); err == nil && !strings.HasSuffix(name, "_t.jpg") {
			n++
		}
		return nil
	})
	return n, err
}
