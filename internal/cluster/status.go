package cluster

import (
	"slices"
	"sort"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Network status, for the admin page's Network page (/admin/network).
//
// Each node puts a short account of its own health in the report it swaps
// with every other node (NodeStatus). So whichever node serves the page
// knows not only who it hears from, but who every other node hears from,
// and can show the whole network: a link that's down between two other
// nodes shows up here too. What it knows of another node is as old as that
// node's last report, which the page shows.

// NodeStatus is what a node says about its own health, in its report.
type NodeStatus struct {
	Version   string `json:"version"`
	Started   int64  `json:"started"` // unix seconds
	PublicIP  string `json:"public_ip,omitempty"`
	Reachable bool   `json:"reachable"`          // others can connect to it
	Refusing  string `json:"refusing,omitempty"` // why it won't take writes
	Rewinds   int64  `json:"rewinds"`
	// Heard: for each other node it has heard from, how long ago, in ms.
	Heard map[string]int64 `json:"heard"`
	// Waiting: for each file it holds, how many operations aren't final
	// yet, and the age in ms of the point up to which they are (0 when
	// that can't be said yet: it's waiting to hear from some node).
	Waiting map[string]int   `json:"waiting"`
	Stable  map[string]int64 `json:"stable"`
}

// statusEvery is how often a node re-reads its own status. Counting each
// file's waiting operations is a query per file, so not every tick.
const statusEvery = 5 * time.Second

// refreshStatus re-reads this node's status, when it's due. Called from the
// upkeep loop.
func (n *Node) refreshStatus() {
	if s := n.status.Load(); s != nil && time.Since(n.statusAt) < statusEvery {
		return
	}
	n.statusAt = time.Now()
	s := &NodeStatus{Version: n.o.Version, Started: n.started.Unix(), Rewinds: n.rewinds.Load(),
		Heard: map[string]int64{}, Waiting: map[string]int{}, Stable: map[string]int64{}}
	n.where.mu.Lock()
	s.PublicIP, s.Reachable = n.where.publicIP, n.where.dialable != ""
	n.where.mu.Unlock()
	s.Refusing, _ = n.refuse.Load().(string)
	n.peersMu.Lock()
	for id, p := range n.peers {
		s.Heard[id] = time.Since(p.heard).Milliseconds()
	}
	n.peersMu.Unlock()
	for _, l := range n.heldLogs() {
		e := n.engineFor(l)
		e.mu.Lock()
		// prepare first, as everything reading a file's stable copy must:
		// store.Stable makes an empty one if there's none, and a file just
		// upgraded from Raft gets its first stable copy from its live file
		// only if none exists yet. Reading it here first would leave an
		// empty stable copy, which other nodes would then copy.
		if err := n.prepare(e); err != nil {
			e.mu.Unlock()
			continue
		}
		point := n.stablePoint(e)
		waiting := -1
		if live, err := n.st.Live(e.gid()); err == nil {
			if stable, err := n.st.Stable(e.gid()); err == nil {
				if spos, err := store.PositionOf(stable); err == nil {
					if c, err := store.CountOpsAfter(live, spos); err == nil {
						waiting = c
					}
				}
			}
		}
		e.mu.Unlock()
		s.Waiting[l.String()] = waiting
		if point > 0 {
			// A stamp is milliseconds << 16 (clock.go).
			s.Stable[l.String()] = time.Now().UnixMilli() - point>>16
		}
	}
	n.status.Store(s)
}

// Network is the whole network as this node knows it.
type Network struct {
	Self  string    // the node that answered
	Nodes []NetNode // the node map, plus any node heard from that isn't in it
	// Heard[i][j]: how long since Nodes[i] last heard from Nodes[j], as
	// Nodes[i] last said.
	Heard [][]NetLink
	Files []NetFile
}

// NetNode is one node.
type NetNode struct {
	store.Node
	InMap  bool        // listed in the node map (false: heard from, but not listed)
	Status *NodeStatus // its own account; nil if it hasn't given one
	// Age: how old what's known of it is (its last report here); 0 for
	// this node. Never: nothing heard from it at all.
	Age   time.Duration
	Never bool
	Fresh bool // heard from lately (always, for this node)
}

// NetLink is one cell of the who-hears-whom grid.
type NetLink struct {
	Self  bool // a node and itself
	Known bool // the row's node has said
	Heard bool // it has heard from the column's node at all
	Ago   time.Duration
	OK    bool // lately
}

// NetFile is one file, and how far along each node holding it is.
type NetFile struct {
	Log     cmd.LogID
	Holders []NetHolder
}

// NetHolder is one node's copy of a file.
type NetHolder struct {
	ID string
	// Reported: the node's report lists this file (false: it's placed there
	// but still being copied, or nothing's been heard from it).
	Reported bool
	// Behind: operations others have that it hasn't, as of its last report.
	Behind int64
	// Waiting and StableAge: from its own status (see NodeStatus); -1 and
	// 0 when not known.
	Waiting   int
	StableAge time.Duration
}

// fresh is how recently a node must have been heard from to count as in
// touch: the same allowance catch-up uses.
func (n *Node) fresh(d time.Duration) bool { return d <= 3*n.o.tick+5*time.Second }

// Network builds the network's status for the admin page.
func (n *Node) Network() Network {
	net := Network{Self: n.o.ID}
	// This node's report as it would send it now. Its status part is the
	// upkeep loop's latest (a few seconds old at most).
	mine, _ := n.report()

	// Every node's latest report, this one's included.
	reports := map[string]*Report{}
	heardAt := map[string]time.Time{}
	n.peersMu.Lock()
	for id, p := range n.peers {
		if p.report != nil {
			reports[id], heardAt[id] = p.report, p.heard
		}
	}
	n.peersMu.Unlock()
	if mine != nil {
		reports[n.o.ID], heardAt[n.o.ID] = mine, time.Now()
	}

	nodes, _ := n.st.Nodes()
	listed := map[string]bool{}
	for _, nd := range nodes {
		listed[nd.ID] = true
		net.Nodes = append(net.Nodes, NetNode{Node: nd, InMap: true})
	}
	for id, r := range reports {
		if !listed[id] {
			net.Nodes = append(net.Nodes, NetNode{Node: store.Node{ID: id, Addr: r.Addr, Num: -1}})
		}
	}
	sort.Slice(net.Nodes, func(i, j int) bool { return net.Nodes[i].ID < net.Nodes[j].ID })
	for i := range net.Nodes {
		nd := &net.Nodes[i]
		r, ok := reports[nd.ID]
		if !ok {
			nd.Never = nd.ID != n.o.ID
			nd.Fresh = nd.ID == n.o.ID
			continue
		}
		nd.Status = r.Status
		if nd.ID != n.o.ID {
			nd.Age = time.Since(heardAt[nd.ID])
		}
		nd.Fresh = n.fresh(nd.Age)
	}

	// Who hears whom.
	for _, row := range net.Nodes {
		var cells []NetLink
		for _, col := range net.Nodes {
			c := NetLink{Self: row.ID == col.ID}
			if !c.Self && row.Status != nil {
				c.Known = true
				if ms, ok := row.Status.Heard[col.ID]; ok {
					// What it said, plus how old its saying is.
					c.Heard, c.Ago = true, time.Duration(ms)*time.Millisecond+row.Age
					c.OK = n.fresh(c.Ago)
				}
			}
			cells = append(cells, c)
		}
		net.Heard = append(net.Heard, cells)
	}

	// Files: every file anyone holds, with who should and who does.
	logs := map[cmd.LogID]bool{}
	for _, r := range reports {
		for name := range r.Logs {
			if l, err := cmd.ParseLogID(name); err == nil {
				logs[l] = true
			}
		}
	}
	for _, l := range sortedLogs(logs) {
		f := NetFile{Log: l}
		ids := n.holderIDs(l)
		for id, r := range reports {
			if _, ok := r.Logs[l.String()]; ok && !slices.Contains(ids, id) {
				ids = append(ids, id) // still holds it (taken off, not yet deleted)
			}
		}
		sort.Strings(ids)
		// The most anyone has from each origin.
		most := map[string]int64{}
		for _, r := range reports {
			for o, seq := range r.Logs[l.String()].Have {
				most[o] = max(most[o], seq)
			}
		}
		for _, id := range ids {
			h := NetHolder{ID: id, Waiting: -1}
			if r, ok := reports[id]; ok {
				if lr, ok := r.Logs[l.String()]; ok {
					h.Reported = true
					for o, seq := range most {
						h.Behind += seq - lr.Have[o]
					}
				}
				if s := r.Status; s != nil {
					if w, ok := s.Waiting[l.String()]; ok {
						h.Waiting = w
					}
					h.StableAge = time.Duration(s.Stable[l.String()]) * time.Millisecond
				}
			}
			f.Holders = append(f.Holders, h)
		}
		net.Files = append(net.Files, f)
	}
	return net
}

func sortedLogs(m map[cmd.LogID]bool) []cmd.LogID {
	var out []cmd.LogID
	for l := range m {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}
