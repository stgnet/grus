// Package cluster is the replicated command log: every write in the system
// goes through Log.Apply.
//
// In production that's Raft (Node, in raft.go): the leader appends the
// command to the log, replicates it to every node, and each node applies it
// to its own SQLite files through the FSM. In M0 the cluster is one VPS
// (the only voter, so always the leader) plus the Studio as a non-voting
// full copy. Later milestones split it into one log per group; handlers
// don't change, because all they ever see is this interface.
package cluster

import (
	"sync"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Log is where writes go. Apply returns once the command has been applied
// on this node, with the command's result.
type Log interface {
	Apply(c cmd.Command) (any, error)
}

// Local applies commands directly to one store, with no replication. Tests
// and single-shot tools use it; a running server always uses Raft.
type Local struct {
	mu    sync.Mutex
	st    *store.Store
	index uint64
}

// NewLocal returns a Local log that continues numbering after whatever the
// store has already applied.
func NewLocal(st *store.Store) (*Local, error) {
	idx, err := st.MaxApplied()
	if err != nil {
		return nil, err
	}
	return &Local{st: st, index: idx}, nil
}

// Apply runs one command. Commands are applied one at a time, as they are
// by Raft, so Apply code never has to think about concurrency.
func (l *Local) Apply(c cmd.Command) (any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.index++
	return cmd.Run(l.st, l.index, c)
}
