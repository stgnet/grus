package cmd

import (
	"database/sql"
	"sort"
	"strings"
)

// The node map (plan section 8): which nodes there are, and which groups
// each holds. It lives in site.db, so every node knows where every group
// is, and it's changed only through the site log, so every node agrees.
// Each log's leader keeps its Raft membership matching it (internal/cluster).

// GroupVoters is how many voters a new group gets: three survive losing
// one. With fewer voter nodes, it gets all of them.
const GroupVoters = 3

// RegisterNode records a node, or updates its address and roles. Every
// node submits one when it starts, from its config file, so the map
// follows the config. A new full node is placed on every group.
type RegisterNode struct {
	ID     string
	Addr   string
	Voter  bool
	Full   bool
	AI     bool   // runs a model
	Origin string // its current origin (id and incarnation), for replication
	Num    int    // its node number (internal/ids); -1 if not known
	At     int64
}

func (c *RegisterNode) Apply(a *Applier) (any, error) {
	if c.ID == "" || c.Addr == "" || strings.ContainsAny(c.ID, " /") {
		return nil, Invalid("a node needs an id and an address")
	}
	return nil, a.Site(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO nodes (id, addr, voter, full, ai, origin, num, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET addr = excluded.addr, voter = excluded.voter, full = excluded.full,
			  ai = excluded.ai, origin = excluded.origin, num = excluded.num, updated_at = excluded.updated_at`,
			c.ID, c.Addr, c.Voter, c.Full, c.AI, c.Origin, c.Num, c.At, c.At); err != nil {
			return err
		}
		if c.Voter {
			// A group with no host at all (made before any node had
			// registered: the first node's own first groups) gets this
			// voter, which starts its log.
			if _, err := tx.Exec(`INSERT INTO group_hosts (group_id, node_id, voter, bootstrap, created_at)
				SELECT id, ?, 1, 1, ? FROM groups WHERE id NOT IN (SELECT group_id FROM group_hosts)`, c.ID, c.At); err != nil {
				return err
			}
		}
		if !c.Full {
			return nil
		}
		// A full node holds everything: every group it isn't on yet, as a
		// non-voter (joining each group's running log).
		_, err := tx.Exec(`INSERT OR IGNORE INTO group_hosts (group_id, node_id, voter, bootstrap, created_at)
			SELECT id, ?, 0, 0, ? FROM groups`, c.ID, c.At)
		return err
	})
}

// RemoveNode takes a node out of the map, and off every group it held.
// It's for a node that's gone for good: the stable point stops waiting
// for it (docs/replication.md). A group left with no primary host gets
// Replacement (another node's id), so no group is stranded.
//
// Upto is how many of the node's operations each file had here when it
// was removed, by log name ("site", "g42"), for its current origin.
// Anything more from that origin is ignored everywhere from then on; if
// the node was not gone after all, it finds itself removed when it comes
// back and sends those again under a new origin.
type RemoveNode struct {
	ID          string
	Replacement string
	Origin      string
	Upto        map[string]int64
	At          int64
}

func (c *RemoveNode) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		// In log-name order, so every node writes the rows the same way.
		logs := make([]string, 0, len(c.Upto))
		for l := range c.Upto {
			logs = append(logs, l)
		}
		sort.Strings(logs)
		for _, l := range logs {
			if _, err := tx.Exec(`INSERT INTO removed_origins (origin, log, upto) VALUES (?, ?, ?)
				ON CONFLICT (origin, log) DO UPDATE SET upto = MAX(upto, excluded.upto)`, c.Origin, l, c.Upto[l]); err != nil {
				return err
			}
		}
		if c.Replacement != "" {
			var n int
			tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ? AND id != ?`, c.Replacement, c.ID).Scan(&n)
			if n == 0 {
				return Invalid("no node %s to take its groups", c.Replacement)
			}
			// Groups whose only voter was this node. The replacement is
			// marked bootstrap: if it has the group's log, that changes
			// nothing, and if it hasn't, starting the log afresh is the
			// only way the group can carry on.
			if _, err := tx.Exec(`INSERT INTO group_hosts (group_id, node_id, voter, bootstrap, created_at)
				SELECT h.group_id, ?1, 1, 1, ?3 FROM group_hosts h WHERE h.node_id = ?2 AND h.voter = 1
				  AND NOT EXISTS (SELECT 1 FROM group_hosts o WHERE o.group_id = h.group_id AND o.voter = 1 AND o.node_id != ?2)
				ON CONFLICT (group_id, node_id) DO UPDATE SET voter = 1, bootstrap = 1`, c.Replacement, c.ID, c.At); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM group_hosts WHERE node_id = ?`, c.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, c.ID)
		return err
	})
}

// PlaceGroup puts a group on a node (as a voter or not), or changes its
// part there. The node copies the group from its log's leader and catches
// up; after that it serves the group from its own copy.
type PlaceGroup struct {
	GroupID int64
	NodeID  string
	Voter   bool
	At      int64
}

func (*PlaceGroup) siteLog() {}

func (c *PlaceGroup) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		var n int
		tx.QueryRow(`SELECT (SELECT COUNT(*) FROM nodes WHERE id = ?) * (SELECT COUNT(*) FROM groups WHERE id = ?)`, c.NodeID, c.GroupID).Scan(&n)
		if n == 0 {
			return Invalid("no such node or group")
		}
		_, err := tx.Exec(`INSERT INTO group_hosts (group_id, node_id, voter, bootstrap, created_at) VALUES (?, ?, ?, 0, ?)
			ON CONFLICT (group_id, node_id) DO UPDATE SET voter = excluded.voter`, c.GroupID, c.NodeID, c.Voter, c.At)
		return err
	})
}

// UnplaceGroup takes a group off a node. The node leaves the group's log
// and deletes its copy. A group's last voter can't be taken off, and a
// full node holds every group by definition.
type UnplaceGroup struct {
	GroupID int64
	NodeID  string
	At      int64
}

func (*UnplaceGroup) siteLog() {}

func (c *UnplaceGroup) Apply(a *Applier) (any, error) {
	return nil, a.Site(func(tx *sql.Tx) error {
		var voter, full bool
		if err := tx.QueryRow(`SELECT h.voter, n.full FROM group_hosts h JOIN nodes n ON n.id = h.node_id
			WHERE h.group_id = ? AND h.node_id = ?`, c.GroupID, c.NodeID).Scan(&voter, &full); err != nil {
			return Invalid("that node doesn't hold that group")
		}
		if full {
			return Invalid("a full-copy node holds every group")
		}
		if voter {
			var others int
			tx.QueryRow(`SELECT COUNT(*) FROM group_hosts WHERE group_id = ? AND voter = 1 AND node_id != ?`, c.GroupID, c.NodeID).Scan(&others)
			if others == 0 {
				return Invalid("that's the group's last voter; place it on another node first")
			}
		}
		_, err := tx.Exec(`DELETE FROM group_hosts WHERE group_id = ? AND node_id = ?`, c.GroupID, c.NodeID)
		return err
	})
}

// placeNewGroup chooses a new group's hosts: the first GroupVoters voter
// nodes (by id), and every full node. With no voter at all, every node
// holds it, so a group is never placed nowhere. With no nodes registered
// (tests, tools), it places nothing.
func placeNewGroup(tx *sql.Tx, groupID, at int64) error {
	res, err := tx.Exec(`INSERT INTO group_hosts (group_id, node_id, voter, bootstrap, created_at)
		SELECT ?, id, 1, 1, ? FROM nodes WHERE voter = 1 ORDER BY id LIMIT ?`, groupID, at, GroupVoters)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.Exec(`INSERT INTO group_hosts (group_id, node_id, voter, bootstrap, created_at)
			SELECT ?, id, 1, 1, ? FROM nodes`, groupID, at); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO group_hosts (group_id, node_id, voter, bootstrap, created_at)
		SELECT ?, id, 0, 0, ? FROM nodes WHERE full = 1`, groupID, at)
	return err
}
