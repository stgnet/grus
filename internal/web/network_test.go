package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// TestNetworkPageWarnings: what the Network page flags, from a network
// with one of each problem in it.
func TestNetworkPageWarnings(t *testing.T) {
	now := time.Now()
	st := func(v string, heard map[string]int64) *cluster.NodeStatus {
		return &cluster.NodeStatus{Version: v, Started: now.Add(-time.Hour).Unix(), Reachable: true, Heard: heard,
			Waiting: map[string]int{}, Stable: map[string]int64{}}
	}
	nw := cluster.Network{
		Self: "vps",
		Nodes: []cluster.NetNode{
			// The Studio runs the model but nobody can connect to it.
			{Node: store.Node{ID: "studio", Full: true, AI: true}, InMap: true, Fresh: true,
				Status: st("v2", map[string]int64{"vps": 1000})},
			// A node gone quiet ten minutes ago.
			{Node: store.Node{ID: "quiet", Addr: "198.51.100.3:7946"}, InMap: true, Age: 10 * time.Minute,
				Status: st("v2", map[string]int64{})},
			// One that answers but the map doesn't list.
			{Node: store.Node{ID: "stray", Addr: "198.51.100.4:7946"}, Fresh: true, Status: st("v1", nil)},
			{Node: store.Node{ID: "vps", Addr: "198.51.100.1:7946", Voter: true}, InMap: true, Fresh: true,
				Status: st("v2", map[string]int64{"studio": 500})},
		},
		Files: []cluster.NetFile{{Log: cmd.LogID(100), Holders: []cluster.NetHolder{
			{ID: "studio", Reported: true, Waiting: 3, StableAge: 20 * time.Minute},
			{ID: "vps", Reported: true, Behind: 2, Waiting: -1},
			{ID: "quiet", Waiting: -1},
		}}},
	}
	for range nw.Nodes {
		nw.Heard = append(nw.Heard, make([]cluster.NetLink, len(nw.Nodes)))
	}
	d := networkPage(nw, map[cmd.LogID]string{100: "travato"}, now)
	all := strings.Join(d.Warnings, "\n")
	for _, want := range []string{
		"studio runs the model, but other nodes can't connect to it",
		"quiet hasn't been heard from for 10m",
		"stray answers but isn't in the node map",
		"Nodes run different versions (v1 on stray; v2 on quiet, studio, vps)",
		"Changes to travato have only been final up to 20m ago",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no warning %q in:\n%s", want, all)
		}
	}
	if got := d.Files[0].Holders; got[0].Text != "studio: up to date, 3 not final" || got[1].Text != "vps: 2 behind" ||
		got[2].Text != "quiet: not holding it yet" || !got[2].Bad {
		t.Errorf("holders: %+v", got)
	}

	// A healthy network has nothing to flag.
	ok := cluster.Network{Self: "vps", Nodes: []cluster.NetNode{
		{Node: store.Node{ID: "studio", Addr: "203.0.113.9:7946", Full: true, AI: true}, InMap: true, Fresh: true, Status: st("v2", nil)},
		{Node: store.Node{ID: "vps", Addr: "198.51.100.1:7946", Voter: true}, InMap: true, Fresh: true, Status: st("v2", nil)},
	}}
	if d := networkPage(ok, nil, now); len(d.Warnings) != 0 {
		t.Errorf("warnings on a healthy network: %v", d.Warnings)
	}
}
