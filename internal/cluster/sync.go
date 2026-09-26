package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"net/http"
	"os"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// How nodes keep each other's files the same (docs/replication.md,
// "Passing operations on"), over the internal API:
//
//	POST /sync/report          swap reports: the caller's for this node's
//	GET  /sync/report          this node's report (make install checks it)
//	POST /sync/pull            the operations on one file someone hasn't got
//	POST /sync/push            operations someone hasn't got, offered
//	GET  /sync/copy/{log}      this node's stable copy of a file
//	GET  /sync/whoami          the address the caller was seen at (addr.go)
//	POST /sync/dialback        connect back to the caller at an address (addr.go)
//
// Every tick, each node swaps reports with every other node it can reach.
// The reports are how a node knows what the others have (so it can pull
// what it's missing, and push what they are), and how it works out each
// file's stable point. A node nobody can reach (behind a home router) does
// the swapping itself, so everyone still hears from it.

// Report is what a node says about itself.
type Report struct {
	ID     string `json:"id"`
	Origin string `json:"origin"`
	// Clock: the node will never stamp anything at or below it, on a file
	// not listed in Logs (see LogReport.Clock for those).
	Clock int64                `json:"clock"`
	Logs  map[string]LogReport `json:"logs"` // the files it holds, by log name
	// Addr is where others can reach it ("" if they can't: see addr.go).
	Addr string `json:"addr"`
	// Seen, in a report sent back to a caller, is the address the caller's
	// request came from: how a node learns its public IP.
	Seen string `json:"seen,omitempty"`
}

// LogReport is a node's report on one file it holds.
type LogReport struct {
	// Clock and Seq are read together, under the file's lock: the node has
	// made Seq operations on this file, and will never stamp one at or
	// below Clock. So whoever has all Seq of them has everything from this
	// node up to Clock.
	Clock int64            `json:"clock"`
	Seq   int64            `json:"seq"`
	Have  map[string]int64 `json:"have"` // its version vector
	Base  int64            `json:"base"` // see store.Position
}

// report builds this node's report.
func (n *Node) report() (*Report, error) {
	r := &Report{ID: n.o.ID, Origin: n.id.origin(), Clock: n.clock.Now(), Logs: map[string]LogReport{}, Addr: n.dialable()}
	for _, l := range n.heldLogs() {
		e := n.engineFor(l)
		e.mu.Lock()
		lr, err := n.logReport(e)
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		r.Logs[l.String()] = lr
	}
	return r, nil
}

// logReport reads one file's part of the report. Called with e.mu held, so
// no operation is being made on it meanwhile.
func (n *Node) logReport(e *engine) (LogReport, error) {
	live, err := n.st.Live(e.gid())
	if err != nil {
		return LogReport{}, err
	}
	seqs, err := store.Seqs(live)
	if err != nil {
		return LogReport{}, err
	}
	pos, err := store.PositionOf(live)
	if err != nil {
		return LogReport{}, err
	}
	lr := LogReport{Clock: n.clock.Now(), Seq: seqs[n.id.origin()].Seq, Have: map[string]int64{}, Base: pos.Base}
	for o, si := range seqs {
		lr.Have[o] = si.Seq
	}
	return lr, nil
}

// peerState is what this node knows of another: its latest report, and
// when it was last heard from.
type peerState struct {
	report *Report
	heard  time.Time
	seen   string // the address it saw this node at, when it said
}

func (n *Node) peer(id string) peerState {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	return n.peers[id]
}

func (n *Node) setPeer(id string, r *Report) {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	seen := r.Seen
	if seen == "" {
		seen = n.peers[id].seen // a report pushed to us says nothing of that
	}
	n.peers[id] = peerState{report: r, heard: time.Now(), seen: seen}
}

// stablePoint is the stamp at or below which no operation on e's file can
// ever arrive again, or 0 if that can't be said yet. For every node in the
// map, it takes the highest stamp up to which this node is sure to have
// everything that node made (from its report); the smallest of those is
// the answer. A node that hasn't reported holds it at 0: the stable point
// waits for every node in the map, so nothing is treated as final that
// isn't. Called with e.mu held.
func (n *Node) stablePoint(e *engine) int64 {
	live, err := n.st.Live(e.gid())
	if err != nil {
		return 0
	}
	mine, err := store.Seqs(live)
	if err != nil {
		return 0
	}
	nodes, err := n.st.Nodes()
	if err != nil {
		return 0
	}
	// This node: its next operation will be stamped after now.
	f := n.clock.Now()
	name := e.log.String()
	for _, nd := range nodes {
		if nd.ID == n.o.ID {
			continue
		}
		p := n.peer(nd.ID)
		if p.report == nil {
			return 0
		}
		r := p.report
		clockAt, made := r.Clock, int64(0)
		lr, holds := r.Logs[name]
		if holds {
			clockAt, made = lr.Clock, lr.Seq
		}
		bound := clockAt
		if have := mine[r.Origin]; have.Seq < made {
			// Still missing some of its operations: sure only up to the
			// last one here from it.
			bound = have.Stamp
		}
		f = min(f, bound)
		// Operations it has from anyone else that this node hasn't got
		// yet are stamped after the last one here from that origin.
		if holds {
			for o, seq := range lr.Have {
				if have := mine[o]; have.Seq < seq {
					f = min(f, have.Stamp)
				}
			}
		}
	}
	return f
}

// pullRequest asks for the operations on one file that the asker hasn't got.
type pullRequest struct {
	Log  string           `json:"log"`
	Have map[string]int64 `json:"have"`
}

type pullReply struct {
	Ops []*cmd.Op `json:"ops"`
	// Gap: some operations the asker needs have been deleted here (every
	// node that needed them had them); it needs a copy of the file.
	Gap bool `json:"gap"`
}

type pushRequest struct {
	Log string    `json:"log"`
	Ops []*cmd.Op `json:"ops"`
}

// pullBatch is the most operations sent in one reply.
const pullBatch = 2000

// serveSync adds the sync endpoints to the internal API.
func (n *Node) serveSync(mux *http.ServeMux) {
	mux.HandleFunc("GET /sync/report", func(w http.ResponseWriter, r *http.Request) {
		rep, err := n.report()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rep.Seen = remoteIP(r)
		writeJSON(w, rep)
	})
	mux.HandleFunc("POST /sync/report", func(w http.ResponseWriter, r *http.Request) {
		var theirs Report
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&theirs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A node can only speak for itself: its certificate names it.
		if theirs.ID == "" || theirs.ID != callerID(r) {
			http.Error(w, "a report from someone else", http.StatusForbidden)
			return
		}
		theirs.Seen = ""
		n.clock.Observe(theirs.Clock)
		n.setPeer(theirs.ID, &theirs)
		rep, err := n.report()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rep.Seen = remoteIP(r)
		writeJSON(w, rep)
	})
	// The ways into the site, for a new node's join lines (make install's
	// deploy/remote-setup.sh asks the node it runs on): every node that
	// can be reached, and every domain (each leads to a node that serves
	// pages, which listens on the cluster port too).
	mux.HandleFunc("GET /sync/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, port, _ := net.SplitHostPort(n.o.Listen)
		nodes, _ := n.st.Nodes()
		for _, nd := range nodes {
			if nd.Addr != "" {
				fmt.Fprintln(w, nd.Addr)
			}
		}
		domains, _ := n.st.Domains()
		for _, d := range domains {
			fmt.Fprintln(w, net.JoinHostPort(d.Name, port))
		}
	})
	mux.HandleFunc("GET /sync/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, whoami{ID: n.o.ID, Seen: remoteIP(r)})
	})
	mux.HandleFunc("POST /sync/dialback", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Addr string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Connect to the caller at the address it gave, and check it's the
		// caller that answers there (so this can't be pointed at anyone
		// else's port).
		var rep Report
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err := n.client.GetJSON(ctx, req.Addr, "/sync/report", &rep)
		cancel()
		writeJSON(w, struct{ OK bool }{err == nil && rep.ID != "" && rep.ID == callerID(r)})
	})
	mux.HandleFunc("POST /sync/pull", func(w http.ResponseWriter, r *http.Request) {
		var req pullRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		l, err := cmd.ParseLogID(req.Log)
		if err != nil || !n.holds(l) {
			http.NotFound(w, r)
			return
		}
		live, err := n.st.Live(int64(l))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows, gap, err := store.OpsNewerThan(live, req.Have, pullBatch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rep := pullReply{Gap: gap}
		for _, row := range rows {
			rep.Ops = append(rep.Ops, toOp(row))
		}
		writeJSON(w, rep)
	})
	mux.HandleFunc("POST /sync/push", func(w http.ResponseWriter, r *http.Request) {
		var req pushRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		l, err := cmd.ParseLogID(req.Log)
		if err != nil || !n.holds(l) {
			http.NotFound(w, r)
			return
		}
		if _, err := n.ingest(n.engineFor(l), req.Ops); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		n.poke()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /sync/copy/{log}", func(w http.ResponseWriter, r *http.Request) {
		l, err := cmd.ParseLogID(r.PathValue("log"))
		if err != nil || !n.holds(l) || (l != cmd.SiteLog && !n.st.HasGroup(int64(l))) {
			http.NotFound(w, r)
			return
		}
		e := n.engineFor(l)
		e.mu.Lock()
		err = n.prepare(e)
		var path string
		if err == nil {
			path, err = n.st.CopyStable(e.gid())
		}
		e.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(path)
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, f)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// swapReports sends this node's report to every other node it can reach,
// and takes theirs back, all at once. A node that can't be reached (no
// address) sends its own; it's heard from that way. If nobody in the map
// answers (their addresses all changed while this node was away, say),
// it tries the nodes the site's domains lead to.
func (n *Node) swapReports(ctx context.Context) {
	nodes, err := n.st.Nodes()
	if err != nil {
		return
	}
	mine, err := n.report()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	answered, others := 0, 0
	for _, nd := range nodes {
		if nd.ID == n.o.ID {
			continue
		}
		others++
		if nd.Addr == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n.swapWith(ctx, nd.Addr, nd.ID, mine) {
				mu.Lock()
				answered++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if others > 0 && answered == 0 {
		n.swapViaDomains(ctx, mine)
	}
}

// swapWith swaps reports with the node at addr, which should be id (""
// for whoever answers). It reports whether that worked.
func (n *Node) swapWith(ctx context.Context, addr, id string, mine *Report) bool {
	var r Report
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := n.client.PostJSON(cctx, addr, "/sync/report", mine, &r); err != nil {
		return false
	}
	if r.ID == "" || (id != "" && r.ID != id) || r.ID == n.o.ID {
		return false // something else answers at that address now
	}
	n.clock.Observe(r.Clock)
	n.setPeer(r.ID, &r)
	return true
}

// swapViaDomains swaps reports with whatever nodes the site's domains
// lead to, on the cluster port.
func (n *Node) swapViaDomains(ctx context.Context, mine *Report) {
	domains, err := n.st.Domains()
	if err != nil {
		return
	}
	_, port, _ := net.SplitHostPort(n.o.Listen)
	for _, d := range domains {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err := net.DefaultResolver.LookupHost(lctx, d.Name)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range ips {
			n.swapWith(ctx, net.JoinHostPort(ip, port), "", mine)
		}
	}
}

// catchUp pulls, from every node whose report shows it has operations on
// a file this node holds that this node hasn't got, what's missing. A
// node that has deleted some this node needs sends a copy of the file
// instead. So is a node whose copy has a newer base (see store.Position).
func (n *Node) catchUp(ctx context.Context) {
	nodes, err := n.st.Nodes()
	if err != nil {
		return
	}
	for _, l := range n.heldLogs() {
		e := n.engineFor(l)
		for _, nd := range nodes {
			if nd.ID == n.o.ID || ctx.Err() != nil {
				continue
			}
			p := n.peer(nd.ID)
			if p.report == nil || time.Since(p.heard) > 3*n.o.tick+5*time.Second || nd.Addr == "" {
				// Not heard from this pass, or it can't be reached: it
				// pushes to this node instead.
				continue
			}
			lr, ok := p.report.Logs[l.String()]
			if !ok {
				continue
			}
			if err := n.catchUpFrom(ctx, e, nd.Addr, lr); err != nil {
				n.warn("catchup "+l.String()+nd.ID, "catching up %s from %s: %v", l, nd.ID, err)
			}
			if err := n.pushMissing(ctx, e, nd.Addr, lr); err != nil {
				n.warn("pushing "+l.String()+nd.ID, "sending %s to %s: %v", l, nd.ID, err)
			}
		}
	}
}

func (n *Node) catchUpFrom(ctx context.Context, e *engine, addr string, lr LogReport) error {
	for round := 0; round < 50; round++ {
		live, err := n.st.Live(e.gid())
		if err != nil {
			return err
		}
		pos, err := store.PositionOf(live)
		if err != nil {
			return err
		}
		if lr.Base > pos.Base {
			// Its copy starts from a later point than this one (it was
			// ahead when both were upgraded from Raft, or this file is
			// new here): take its copy, keeping anything made here.
			return n.fetchCopy(ctx, e, addr)
		}
		mine, err := store.Seqs(live)
		if err != nil {
			return err
		}
		have := map[string]int64{}
		missing := false
		for o, si := range mine {
			have[o] = si.Seq
		}
		for o, seq := range lr.Have {
			if seq > have[o] {
				missing = true
			}
		}
		if !missing {
			return nil
		}
		var rep pullReply
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = n.client.PostJSON(cctx, addr, "/sync/pull", pullRequest{Log: e.log.String(), Have: have}, &rep)
		cancel()
		if err != nil {
			return err
		}
		if rep.Gap {
			return n.fetchCopy(ctx, e, addr)
		}
		if len(rep.Ops) == 0 {
			return nil
		}
		took, err := n.ingest(e, rep.Ops)
		if err != nil {
			return err
		}
		if took == 0 {
			return nil // nothing usable (a removed node's, say): stop asking
		}
		lr.Base = 0 // checked once is enough
	}
	return nil
}

// pushMissing sends a node the operations on e's file its report shows it
// hasn't got. With pulls, that makes everything flow both ways whichever
// of the two can reach the other. (Operations it needs that have been
// deleted here, it gets as a copy of the file from someone.)
func (n *Node) pushMissing(ctx context.Context, e *engine, addr string, lr LogReport) error {
	live, err := n.st.Live(e.gid())
	if err != nil {
		return err
	}
	rows, gap, err := store.OpsNewerThan(live, lr.Have, pullBatch)
	if err != nil || gap || len(rows) == 0 {
		return err
	}
	req := pushRequest{Log: e.log.String()}
	for _, r := range rows {
		req.Ops = append(req.Ops, toOp(r))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return n.client.postRaw(cctx, addr, "/sync/push", body)
}

// fetchCopy takes another node's copy of a file (see installCopy).
func (n *Node) fetchCopy(ctx context.Context, e *engine, addr string) error {
	path, err := n.st.TempPath(e.gid())
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	err = n.client.GetFile(cctx, addr, "/sync/copy/"+e.log.String(), path)
	if err != nil {
		os.Remove(path)
		return err
	}
	logf("copied %s from %s", e.log, addr)
	return n.installCopy(e, path)
}

// errNoCopy means a node asked for a copy of a file doesn't hold it.
var errNoCopy = errors.New("that node doesn't hold the file")

// push offers a new operation to the other nodes that hold its file, and
// waits until one of them has it or ackWait passes, whichever is first.
// It never blocks longer than that, and with no other holder it returns
// at once: the operation is already safe on this node, and pulls carry
// it on from here.
func (n *Node) push(l cmd.LogID, op *cmd.Op) {
	addrs := n.holderAddrs(l)
	if len(addrs) == 0 {
		return
	}
	body, err := json.Marshal(pushRequest{Log: l.String(), Ops: []*cmd.Op{op}})
	if err != nil {
		return
	}
	acked := make(chan struct{}, len(addrs))
	for _, addr := range addrs {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if n.client.postRaw(ctx, addr, "/sync/push", body) == nil {
				acked <- struct{}{}
			}
		}()
	}
	select {
	case <-acked:
	case <-time.After(n.o.ackWait):
	}
}

// holderAddrs lists the cluster addresses of the other nodes holding a file.
func (n *Node) holderAddrs(l cmd.LogID) []string {
	nodes, err := n.st.Nodes()
	if err != nil {
		return nil
	}
	var out []string
	if isEverywhere(l) {
		for _, nd := range nodes {
			if nd.ID != n.o.ID && nd.Addr != "" {
				out = append(out, nd.Addr)
			}
		}
		return out
	}
	hosts, err := n.st.GroupHosts(int64(l))
	if err != nil {
		return nil
	}
	for _, h := range hosts {
		if h.NodeID != n.o.ID && h.Addr != "" {
			out = append(out, h.Addr)
		}
	}
	return out
}

// postRaw posts a JSON body and checks for success.
func (c *Client) postRaw(ctx context.Context, addr, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", addr, path, resp.Status)
	}
	return nil
}

// GetFile downloads a file from another node's internal API to path.
func (c *Client) GetFile(ctx context.Context, addr, path, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
	if err != nil {
		return err
	}
	// No overall timeout from the client: a big group takes a while; ctx
	// bounds it instead.
	hc := *c.hc
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoCopy
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", addr, path, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// prune deletes operations on e's file that every node holding it has,
// and that the stable copy has applied (a rewind never needs them again).
func (n *Node) prune(e *engine) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	live, err := n.st.Live(e.gid())
	if err != nil {
		return err
	}
	stable, err := n.st.Stable(e.gid())
	if err != nil {
		return err
	}
	applied, err := store.Seqs(stable)
	if err != nil {
		return err
	}
	upto := map[string]int64{}
	for o, si := range applied {
		upto[o] = si.Seq
	}
	name := e.log.String()
	holders := n.holderIDs(e.log)
	for _, id := range holders {
		if id == n.o.ID {
			continue
		}
		r := n.peer(id).report
		if r == nil {
			return nil // not heard from: it may still need any of them
		}
		lr, ok := r.Logs[name]
		if !ok {
			return nil // it will need a copy, or is about to pull
		}
		for o := range upto {
			upto[o] = min(upto[o], lr.Have[o])
		}
	}
	for o, seq := range upto {
		if seq <= 0 {
			delete(upto, o)
		}
	}
	if len(upto) == 0 {
		return nil
	}
	if err := store.PruneOps(live, upto); err != nil {
		return err
	}
	return store.PruneOps(stable, upto)
}

// holderIDs lists the ids of the nodes holding a file, this one included.
func (n *Node) holderIDs(l cmd.LogID) []string {
	var out []string
	if isEverywhere(l) {
		nodes, _ := n.st.Nodes()
		for _, nd := range nodes {
			out = append(out, nd.ID)
		}
		return out
	}
	hosts, _ := n.st.GroupHosts(int64(l))
	for _, h := range hosts {
		out = append(out, h.NodeID)
	}
	return out
}

// onDutyAmong reports whether this node is on duty among ids: the one
// with the lowest id among those heard from in the last dutyWindow (and
// this node). See docs/replication.md, "Duties".
func (n *Node) onDutyAmong(ids []string) bool {
	best := n.o.ID
	for _, id := range ids {
		if id >= best {
			continue
		}
		if p := n.peer(id); !p.heard.IsZero() && time.Since(p.heard) < dutyWindow {
			best = id
		}
	}
	return best == n.o.ID
}

const dutyWindow = time.Minute
