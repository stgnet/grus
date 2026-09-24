package cmd

import "github.com/stgnet/grus/internal/store"

// Direct applies commands straight to a store, with no replication: the
// engine of cluster.Local (tests and single-shot tools) and of this
// package's own tests. It isn't safe for concurrent use; Local adds the
// lock.
//
// It keeps one index per log, as Raft does, and relays outboxes itself,
// straight after each command, so a caller sees a command's follow-ups
// (in other groups, or in site.db) as soon as Apply returns.
type Direct struct {
	Store *store.Store
	index map[LogID]uint64
}

// Apply runs one command and everything it sends on.
func (d *Direct) Apply(c Command) (any, error) {
	v, err := d.run(c)
	if err != nil {
		return nil, err
	}
	return v, d.drain(LogOf(c))
}

func (d *Direct) run(c Command) (any, error) {
	if d.index == nil {
		d.index = map[LogID]uint64{}
	}
	log := LogOf(c)
	if _, ok := d.index[log]; !ok {
		idx, err := d.Store.FileApplied(int64(log))
		if err != nil {
			return nil, err
		}
		d.index[log] = idx
	}
	d.index[log]++
	return Run(d.Store, log, d.index[log], c)
}

// drain relays a log's outbox, and the outboxes of whatever that sets
// off, until they're all empty. A follow-up that fails with the person's
// own kind of error (the group it was for is gone, say) is dropped, as the
// cluster's relay does; any other failure stops the drain and is returned.
func (d *Direct) drain(from LogID) error {
	pending := []LogID{from}
	for len(pending) > 0 {
		log := pending[0]
		pending = pending[1:]
		if log != SiteLog && !d.Store.HasGroup(int64(log)) {
			continue
		}
		items, err := d.Store.Outbox(int64(log), 1000)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			continue
		}
		for _, it := range items {
			fc, err := Decode(it.Command)
			if err != nil {
				return err
			}
			if _, err := d.run(fc); err != nil && !IsInput(err) {
				return err
			}
			pending = append(pending, LogOf(fc))
		}
		if _, err := d.run(&OutboxDone{GroupID: int64(log), UpTo: items[len(items)-1].ID}); err != nil {
			return err
		}
		pending = append(pending, log) // anything more it queued meanwhile
	}
	return nil
}
