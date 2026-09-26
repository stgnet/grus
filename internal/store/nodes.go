package store

// The node map (plan section 8): the nodes, and which groups each holds.
// It's in site.db, changed only through the site log (internal/cmd,
// nodes.go), so every node has the same map.

// Node is one server in the cluster.
type Node struct {
	ID    string
	Addr  string // cluster host:port
	Voter bool
	Full  bool
	AI    bool // runs a model: searches and background jobs can go to it
}

// Host is one node's part in one group.
type Host struct {
	GroupID   int64
	NodeID    string
	Addr      string
	Voter     bool
	Bootstrap bool // one of the group's first voters, which start its log
}

// Nodes lists every node, by id.
func (s *Store) Nodes() ([]Node, error) {
	rows, err := s.Site().Query(`SELECT id, addr, voter, full, ai FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Addr, &n.Voter, &n.Full, &n.AI); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GroupHosts lists the nodes holding a group (groupID > 0), or, with
// groupID 0, every placement on every group.
func (s *Store) GroupHosts(groupID int64) ([]Host, error) {
	q := `SELECT h.group_id, h.node_id, n.addr, h.voter, h.bootstrap
		FROM group_hosts h JOIN nodes n ON n.id = h.node_id`
	args := []any{}
	if groupID != 0 {
		q += ` WHERE h.group_id = ?`
		args = append(args, groupID)
	}
	rows, err := s.Site().Query(q+` ORDER BY h.group_id, h.node_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.GroupID, &h.NodeID, &h.Addr, &h.Voter, &h.Bootstrap); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HostedBy lists the groups placed on one node, by group id.
func (s *Store) HostedBy(nodeID string) (map[int64]Host, error) {
	all, err := s.GroupHosts(0)
	if err != nil {
		return nil, err
	}
	out := map[int64]Host{}
	for _, h := range all {
		if h.NodeID == nodeID {
			out[h.GroupID] = h
		}
	}
	return out, nil
}
