package cluster

import (
	"archive/tar"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// fsm is what Raft calls to apply committed log entries to this node's
// SQLite files. ("FSM", finite state machine, is Raft's name for it.)
type fsm struct {
	st *store.Store
}

// result carries a command's return value back to the Apply call that
// submitted it (on the leader; other nodes discard it).
type result struct {
	value any
	err   error
}

func (f *fsm) Apply(entry *raft.Log) any {
	c, err := cmd.Decode(entry.Data)
	if err != nil {
		// A command this binary doesn't know: a newer node wrote it. Applying
		// nothing would let this copy silently diverge, so stop instead; the
		// fix is to upgrade this node.
		log.Fatalf("raft: log entry %d: %v (is this node running an older grus?)", entry.Index, err)
	}
	v, err := cmd.Run(f.st, entry.Index, c)
	return result{v, err}
}

// Snapshot is called by Raft (on its apply goroutine, between entries) to
// let it discard old log entries. It returns quickly; the copying happens in
// Persist, which Raft runs concurrently with new entries being applied.
//
// That concurrency is safe here, and it's worth knowing why: the snapshot is
// labeled with the index it was requested at, but the files copied a moment
// later may already include a few later entries. Each file records the last
// index applied to it, so whoever restores the snapshot skips those entries
// when the log replays them. A snapshot "ahead of its label" is still exact.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{st: f.st}, nil
}

// Restore replaces this node's databases with a snapshot. Raft calls it when
// the leader sends a snapshot to a node too far behind to catch up from the
// log (a new node, or one offline for a long time).
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	tmp, err := os.MkdirTemp(f.st.Dir(), "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := untar(rc, tmp); err != nil {
		return err
	}
	return f.st.Replace(tmp)
}

type fsmSnapshot struct {
	st *store.Store
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := WriteSnapshot(s.st, sink); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// WriteSnapshot writes a consistent copy of every database to w as a tar
// file: site.db and groups/<id>/group.db. Photo blobs aren't included; they
// are content-addressed files synced separately.
func WriteSnapshot(st *store.Store, w io.Writer) error {
	tmp, err := os.MkdirTemp(st.Dir(), "snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := st.CopyTo(tmp); err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	err = filepath.Walk(tmp, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, err := filepath.Rel(tmp, path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o640, Size: fi.Size(), ModTime: fi.ModTime()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// untar unpacks a snapshot into dir, refusing any path that would land
// outside it.
func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("snapshot: bad path %q", hdr.Name)
		}
		out := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}
