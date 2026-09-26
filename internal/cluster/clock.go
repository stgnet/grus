package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The hybrid logical clock that stamps operations (docs/replication.md,
// "Order"). A stamp is wall-clock milliseconds shifted left 16 bits, plus a
// counter in the low bits, and every stamp this node hands out is larger
// than every stamp and clock reading it has seen. So an operation made
// after seeing another sorts after it, however far apart the two nodes'
// clocks are.
//
// The stable point relies on a promise: when this node tells others its
// clock is T, it will never stamp anything at or below T again. That has
// to hold across a restart, when the in-memory clock is gone and the wall
// clock might have been stepped back. So the clock keeps a high-water mark
// on disk, always ahead of anything it has handed out or reported, and
// starts from there.

// stampBits is how far the milliseconds are shifted.
const stampBits = 16

// clock is the hybrid logical clock.
type clock struct {
	mu   sync.Mutex
	last int64            // the largest stamp handed out or seen
	now  func() time.Time // replaced in tests
	id   *identity        // where the high-water mark is kept
}

// Now hands out a new stamp, larger than every one before.
func (c *clock) Now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall := c.now().UnixMilli() << stampBits
	if wall > c.last {
		c.last = wall
	} else {
		c.last++
	}
	c.reserve(c.last)
	return c.last
}

// Observe notes a stamp or clock reading seen from another node.
func (c *clock) Observe(t int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t > c.last {
		c.last = t
		c.reserve(t)
	}
}

// highWaterGap is how far ahead of the clock the saved high-water mark is
// kept, so it's written about once a minute rather than on every stamp.
const highWaterGap = int64(time.Minute/time.Millisecond) << stampBits

// reserve makes sure the saved high-water mark is above t. On a restart
// the clock starts at the mark, so it can never go back below anything
// it handed out or reported.
func (c *clock) reserve(t int64) {
	if c.id == nil {
		return
	}
	c.id.mu.Lock()
	defer c.id.mu.Unlock()
	if t < c.id.HighWater {
		return
	}
	c.id.HighWater = t + highWaterGap
	if err := c.id.save(); err != nil {
		logf("saving the clock's high-water mark: %v", err)
	}
}

// identity is what a node keeps about itself in its data directory
// (node.json), apart from the files: who it is to the replication engine.
type identity struct {
	// Origin is this node's id plus an incarnation, the time its data
	// directory was first used: "n1@1767225600000". A node whose data is
	// lost and rebuilt gets a new one, so its operations can never be
	// mixed up with the ones its old self made.
	Origin    string `json:"origin"`
	HighWater int64  `json:"high_water"` // see clock
	// Num is the node number this node chose for itself, when grus.conf
	// doesn't give one (see Node.chooseNum); nil until then.
	Num  *int `json:"num,omitempty"`
	path string
	mu   sync.Mutex // Origin can change (rejoin) while it's read
}

// origin is the node's current origin.
func (id *identity) origin() string {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.Origin
}

// loadIdentity reads node.json, or makes it for a new data directory, or
// a node id that has changed (which is a new node as far as the others
// are concerned).
func loadIdentity(dir, nodeID string, now time.Time) (*identity, error) {
	id := &identity{path: filepath.Join(dir, "node.json")}
	data, err := os.ReadFile(id.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, id); err != nil {
			return nil, fmt.Errorf("%s: %w", id.path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if originNode(id.Origin) != nodeID {
		id.Origin = fmt.Sprintf("%s@%d", nodeID, now.UnixMilli())
		id.HighWater = 0
		if err := id.save(); err != nil {
			return nil, err
		}
	}
	return id, nil
}

// setNum records the node number this node chose.
func (id *identity) setNum(num int) error {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.Num = &num
	return id.save()
}

// newIncarnation gives the node a new origin, after it found itself
// removed from the map (see rejoin).
func (id *identity) newIncarnation(nodeID string, now time.Time) error {
	id.mu.Lock()
	defer id.mu.Unlock()
	id.Origin = fmt.Sprintf("%s@%d", nodeID, now.UnixMilli())
	return id.save()
}

// save writes node.json. Called with id.mu held (or before anyone else
// can see id).
func (id *identity) save() error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp := id.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// fsync before the rename: the high-water mark is a promise, and a
	// power cut mustn't leave an older one behind.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, id.path)
}

// originNode is the node id part of an origin.
func originNode(origin string) string {
	for i := len(origin) - 1; i >= 0; i-- {
		if origin[i] == '@' {
			return origin[:i]
		}
	}
	return origin
}
