package cluster

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// The replication engine for one file (docs/replication.md). Each file this
// node holds (site.db, the root FAQ's, and each group placed on it) has
// one engine, which is the only thing that writes to that file, one
// operation at a time:
//
//   - originate: a write made here becomes the next operation;
//   - ingest: operations from other nodes are taken in, applied straight
//     on when they come after everything here, or by rewinding when they
//     belong earlier;
//   - stabilize: operations up to the stable point are applied to the
//     stable copy, which rewinds start from.
//
// The two copies (see internal/store/copy.go): the live file, which pages
// read, holds every operation this node has, applied in order; the stable
// copy holds those up to the stable point. Both apply the same operations
// in the same order, so the stable copy is always what the live file was
// at the stable point.

// engine is one file's engine.
type engine struct {
	log cmd.LogID
	mu  sync.Mutex // one change to this file at a time
}

// gid is the file's id in the store's terms: 0 for site.db, else a group.
func (e *engine) gid() int64 { return int64(e.log) }

func (n *Node) engineFor(l cmd.LogID) *engine {
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.engines[l]
	if e == nil {
		e = &engine{log: l}
		n.engines[l] = e
	}
	return e
}

// prepare makes sure the file has its stable copy (see store.InitStable),
// and that this node isn't shut down. Called with e.mu held.
func (n *Node) prepare(e *engine) error {
	if n.closed.Load() {
		return errShutdown
	}
	if n.st.HasStable(e.gid()) {
		return nil
	}
	return n.st.InitStable(e.gid())
}

func toOp(r store.OpRow) *cmd.Op {
	return &cmd.Op{Origin: r.Origin, Seq: r.Seq, Stamp: r.Stamp, Cause: r.Cause, Command: r.Command}
}

// errShutdown is the answer to anything that reaches a node shutting down.
var errShutdown = errors.New("this node is shutting down")

func posOp(p store.Position) *cmd.Op { return &cmd.Op{Stamp: p.Stamp, Origin: p.Origin, Seq: p.Seq} }

// originate makes c this node's next operation on its file and applies
// it, as the one fresh application that decides whether it happens (see
// cmd.Applier.Fresh). A command that fails isn't an operation at all.
// cause is set for a follow-up (see cmd.Op.Cause).
func (n *Node) originate(e *engine, c cmd.Command, cause string) (any, *cmd.Op, error) {
	data, err := cmd.Encode(c)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := n.prepare(e); err != nil {
		return nil, nil, err
	}
	live, err := n.st.Live(e.gid())
	if err != nil {
		return nil, nil, err
	}
	seqs, err := store.Seqs(live)
	if err != nil {
		return nil, nil, err
	}
	origin := n.id.origin()
	op := &cmd.Op{Origin: origin, Seq: seqs[origin].Seq + 1, Stamp: n.clock.Now(), Cause: cause, Command: data}
	// The clock is past everything this node has seen, so the operation
	// sorts after everything in the live file: it's applied straight on.
	v, err := cmd.ApplyOp(n.st, e.log, nil, op, true)
	if err != nil {
		return nil, nil, err
	}
	return v, op, nil
}

// ingest takes in operations from another node. It keeps those it hasn't
// got that follow on from what it has from each origin (a gap is filled by
// the next pull), drops those from a removed node beyond where it was
// removed, and applies the rest in order: straight on if they all come
// after the live file's last operation, by rewinding otherwise. It reports
// how many it took in.
func (n *Node) ingest(e *engine, ops []*cmd.Op) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := n.prepare(e); err != nil {
		return 0, err
	}
	live, err := n.st.Live(e.gid())
	if err != nil {
		return 0, err
	}
	seqs, err := store.Seqs(live)
	if err != nil {
		return 0, err
	}
	removed, err := n.st.RemovedOrigins()
	if err != nil {
		return 0, err
	}
	// Each origin's operations in seq order, taking only an unbroken run
	// from where this file is.
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Origin != ops[j].Origin {
			return ops[i].Origin < ops[j].Origin
		}
		return ops[i].Seq < ops[j].Seq
	})
	next := map[string]int64{}
	var fresh []*cmd.Op
	for _, op := range ops {
		if _, ok := next[op.Origin]; !ok {
			next[op.Origin] = seqs[op.Origin].Seq + 1
		}
		if op.Seq != next[op.Origin] {
			continue // already here, or after a gap
		}
		if gone, ok := removed[op.Origin]; ok && op.Seq > gone[e.log.String()] {
			// From a removed node, beyond where it was removed. (A file
			// with no entry had none of its operations: nothing is taken.)
			continue
		}
		next[op.Origin]++
		fresh = append(fresh, op)
		n.clock.Observe(op.Stamp)
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Less(fresh[j]) })
	pos, err := store.PositionOf(live)
	if err != nil {
		return 0, err
	}
	if !posOp(pos).Less(fresh[0]) {
		return len(fresh), n.rewind(e, fresh)
	}
	for _, op := range fresh {
		if _, err := cmd.ApplyOp(n.st, e.log, nil, op, false); errors.Is(err, cmd.ErrRetry) {
			return 0, err
		}
		// Any other error is the command failing in its place: it's
		// recorded as a conflict, the same on every node, and that's all.
	}
	return len(fresh), nil
}

// rewind rebuilds the live file with extra in its place: a copy of the
// stable file, then every operation after the stable point (those in the
// live file, and extra) in order. Pages switch to the rebuilt file once
// it's complete. Called with e.mu held.
func (n *Node) rewind(e *engine, extra []*cmd.Op) error {
	live, err := n.st.Live(e.gid())
	if err != nil {
		return err
	}
	stable, err := n.st.Stable(e.gid())
	if err != nil {
		return err
	}
	spos, err := store.PositionOf(stable)
	if err != nil {
		return err
	}
	rows, err := store.OpsAfter(live, spos, 0)
	if err != nil {
		return err
	}
	all := append([]*cmd.Op(nil), extra...)
	for _, op := range extra {
		if !posOp(spos).Less(op) {
			// The stable point promised nothing more could come before it.
			// This can only happen if a node broke its promise (its clock
			// high-water mark was lost with its node.json, say) or was
			// removed and its operations were still going round. The
			// operation is applied after the stable point instead, which
			// may differ from where other nodes put it: say so loudly.
			logf("%s: operation %s arrived from before the stable point; applying it after", e.log, op.ID())
		}
	}
	for _, r := range rows {
		all = append(all, toOp(r))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Less(all[j]) })

	path, err := n.st.CopyStable(e.gid())
	if err != nil {
		return err
	}
	db, err := store.OpenCopy(path, e.gid())
	if err != nil {
		os.Remove(path)
		return err
	}
	for _, op := range all {
		if _, err := cmd.ApplyOp(n.st, e.log, db, op, false); errors.Is(err, cmd.ErrRetry) {
			db.Close()
			os.Remove(path)
			return err
		}
	}
	if err := db.Close(); err != nil {
		os.Remove(path)
		return err
	}
	n.rewinds.Add(1)
	return n.st.SwapLive(e.gid(), path)
}

// stabilize applies to the stable copy the live file's operations after
// its position and up to the stable point, in order. Called without e.mu.
func (n *Node) stabilize(e *engine) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := n.prepare(e); err != nil {
		return err
	}
	f := n.stablePoint(e)
	if f == 0 {
		return nil
	}
	live, err := n.st.Live(e.gid())
	if err != nil {
		return err
	}
	stable, err := n.st.Stable(e.gid())
	if err != nil {
		return err
	}
	spos, err := store.PositionOf(stable)
	if err != nil {
		return err
	}
	rows, err := store.OpsAfter(live, spos, f)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := cmd.ApplyOp(n.st, e.log, stable, toOp(r), false); errors.Is(err, cmd.ErrRetry) {
			return err
		}
	}
	return nil
}

// installCopy makes a copy of the file fetched from another node (its
// stable copy, at path) this node's stable and live copies, then takes in
// again every operation the old live file had, so nothing made or received
// here is lost: those the copy already has are skipped. Called without
// e.mu.
func (n *Node) installCopy(e *engine, path string) error {
	e.mu.Lock()
	var keep []*cmd.Op
	if n.st.HasGroup(e.gid()) || e.gid() == 0 {
		if live, err := n.st.Live(e.gid()); err == nil {
			if rows, err := store.OpsAfter(live, store.Position{}, 0); err == nil {
				for _, r := range rows {
					keep = append(keep, toOp(r))
				}
			}
		}
	}
	err := n.st.ReplaceStable(e.gid(), path)
	if err == nil {
		var copyPath string
		copyPath, err = n.st.CopyStable(e.gid())
		if err == nil {
			if err = n.st.SwapLive(e.gid(), copyPath); err != nil {
				os.Remove(copyPath)
			}
		}
	}
	e.mu.Unlock()
	if err != nil {
		return fmt.Errorf("installing a copy of %s: %w", e.log, err)
	}
	if len(keep) > 0 {
		_, err = n.ingest(e, keep)
	}
	return err
}
