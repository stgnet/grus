package cluster

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// fsm is what one log's Raft calls to apply committed entries to the one
// file that log is for: site.db, or a group's file. ("FSM", finite state
// machine, is Raft's name for it.)
type fsm struct {
	st  *store.Store
	log cmd.LogID
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
		log.Fatalf("raft %s: log entry %d: %v (is this node running an older grus?)", f.log, entry.Index, err)
	}
	v, err := cmd.Run(f.st, f.log, entry.Index, c)
	return result{v, err}
}

// Snapshot is called by Raft (on its apply goroutine, between entries) to
// let it discard old log entries. It returns quickly; the copying happens in
// Persist, which Raft runs concurrently with new entries being applied.
//
// That concurrency is safe here, and it's worth knowing why: the snapshot is
// labeled with the index it was requested at, but the file copied a moment
// later may already include a few later entries. The file records the last
// index applied to it, so whoever restores the snapshot skips those entries
// when the log replays them. A snapshot "ahead of its label" is still exact.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{st: f.st, log: f.log}, nil
}

// Restore replaces this node's copy of the file with a snapshot. Raft calls
// it when the leader sends a snapshot to a node too far behind to catch up
// from the log: a node the group was just placed on, or one offline for a
// long time. That's how a new host is seeded.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	tmp, err := os.CreateTemp(f.st.Dir(), "restore-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone already after a successful rename
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return f.st.ReplaceFile(int64(f.log), tmp.Name())
}

type fsmSnapshot struct {
	st  *store.Store
	log cmd.LogID
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := WriteSnapshot(s.st, s.log, sink); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// WriteSnapshot writes a consistent copy of one log's file to w: the SQLite
// file itself, which is all the state that log has. Photo blobs aren't
// included; they're content-addressed files that travel separately.
func WriteSnapshot(st *store.Store, l cmd.LogID, w io.Writer) error {
	dir, err := os.MkdirTemp(st.Dir(), "snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "copy.db")
	if l != cmd.SiteLog {
		// A group log with nothing written to it yet (the root FAQ before
		// anyone edits it) has no file; its snapshot is an empty one.
		if _, err := st.GroupOrCreate(int64(l)); err != nil {
			return err
		}
	}
	if err := st.CopyFile(int64(l), path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
