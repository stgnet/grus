package cluster

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

// Passing web requests on (plan section 8, "reverse-proxy fallback"): a
// node that gets a request for a group it doesn't hold passes it, over the
// cluster port, to a node that does, which serves it as if it had arrived
// there. So DNS can point every group's name at any node, and moving a
// group between nodes needs no DNS change.
//
// The request goes to the other node's internal API under /web/, with its
// Host header kept (that's how the page knows which group it's for) and the
// visitor's address in X-Forwarded-For. Only cluster members can reach
// that API, so the other node can trust both.

// passedHeader marks a request that has already been passed on once, so it
// can never go round in a loop between two nodes that each think the other
// holds the group.
const passedHeader = "X-Grus-Passed"

// PassOn serves a web request from another node that holds groupID, or,
// with groupID 0, from a node holding every group (for the pages that
// gather from all of them). It reports false when there's no such node or
// none could be reached, and the caller serves the page from what it has.
func (n *Node) PassOn(w http.ResponseWriter, r *http.Request, groupID int64) bool {
	if r.Header.Get(passedHeader) != "" {
		return false
	}
	targets := n.holders(groupID)
	if len(targets) == 0 {
		return false
	}
	// Only a request without a body can be tried on a second node: a
	// body is read by the first attempt.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		targets = targets[:1]
	}
	for _, addr := range targets {
		failed := false
		p := &httputil.ReverseProxy{
			Transport: n.client.hc.Transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL = &url.URL{Scheme: "https", Host: addr, Path: "/web" + pr.In.URL.Path, RawQuery: pr.In.URL.RawQuery}
				pr.Out.Host = pr.In.Host // which site and group the page is for
				pr.SetXForwarded()
				pr.Out.Header.Set(passedHeader, n.o.ID)
			},
			// Called before anything is written when the node can't be
			// reached, so the next one can still be tried.
			ErrorHandler: func(http.ResponseWriter, *http.Request, error) { failed = true },
		}
		p.ServeHTTP(w, r)
		if !failed {
			return true
		}
	}
	return false
}

// holders lists the other nodes to pass a group's request to: its voters
// first (they're the likeliest to be current), then its other hosts, then
// full nodes. With groupID 0, only full nodes.
func (n *Node) holders(groupID int64) []string {
	var out []string
	add := func(id, addr string) {
		if id != n.o.ID && addr != "" && !slices.Contains(out, addr) {
			out = append(out, addr)
		}
	}
	if groupID != 0 {
		hosts, _ := n.st.GroupHosts(groupID)
		for _, voter := range []bool{true, false} {
			for _, h := range hosts {
				if h.Voter == voter {
					add(h.NodeID, h.Addr)
				}
			}
		}
	}
	nodes, _ := n.st.Nodes()
	for _, nd := range nodes {
		if nd.Full {
			add(nd.ID, nd.Addr)
		}
	}
	return out
}

// HoldsAll reports whether this node holds every group, so the pages that
// gather from all groups can be served from its own files.
func (n *Node) HoldsAll() bool {
	gs, err := n.st.Groups()
	if err != nil {
		return false
	}
	for _, g := range gs {
		if !n.Holds(g.ID) {
			return false
		}
	}
	return true
}

// BlobPeers lists the other nodes that should have a group's photos (its
// hosts, and full nodes), or every other node with groupID 0.
func (n *Node) BlobPeers(groupID int64) []string {
	if groupID != 0 {
		return n.holders(groupID)
	}
	var out []string
	nodes, _ := n.st.Nodes()
	for _, nd := range nodes {
		if nd.ID != n.o.ID && nd.Addr != "" {
			out = append(out, nd.Addr)
		}
	}
	return out
}

// ServeWeb adds the /web/ endpoint to the node's internal API: requests
// other nodes pass on, served by h (the web server's handler) as if they
// had arrived here directly.
func (n *Node) ServeWeb(h http.Handler) {
	n.RPC().HandleFunc("/web/", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/web")
		r2.RequestURI = r2.URL.RequestURI()
		// The visitor's own address, for per-address limits: the last
		// X-Forwarded-For entry is the one the passing node added.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip := strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:])
			if net.ParseIP(ip) != nil {
				r2.RemoteAddr = net.JoinHostPort(ip, "0")
			}
		}
		h.ServeHTTP(w, r2)
	})
}
