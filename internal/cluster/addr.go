package cluster

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// Where a node can be reached (its address in the node map), found by the
// node itself: nobody types it in, and no node needs a DNS name of its own.
//
//  1. Its public IP. The other nodes each say, in the report they send
//     back, the address they saw this node's request come from. Failing
//     that (a node that has heard from nobody yet), it asks whichever node
//     the site's own domains lead to: every domain in site.db points at a
//     live node that serves pages, and that node answers on the cluster
//     port too (/sync/whoami). The first node on a VPS, whose domain points
//     at itself, asks itself. No outside service is involved.
//  2. Whether it can be reached there: a router or firewall may block the
//     port. It asks another node to connect back to it at that address
//     (/sync/dialback). The first node, alone, checks instead whether the
//     IP is on one of its own network interfaces, as a VPS's is.
//
// A node that can be reached is listed at <public IP>:<cluster port>. One
// that can't (the Studio behind a home router) is listed with no address:
// the others never dial it, and it does all the talking instead. It sends
// its report to each of them and pushes what they're missing, as well as
// pulling what it's missing, so everything still flows both ways. It checks
// again every few minutes, so a new home IP, or a port opened on the
// router, is picked up by itself.
//
// Options.Advertise, when set, is used as it is instead (a private network,
// or a test).

// addrCheckEvery is how often a node looks again at where it can be
// reached.
const addrCheckEvery = 5 * time.Minute

// addrState is what a node knows about its own address.
type addrState struct {
	mu        sync.Mutex
	dialable  string // host:port others can reach it at; "" if none
	publicIP  string
	reachable bool
	checked   time.Time
}

// dialable is this node's address for the node map: "" when others can't
// reach it.
func (n *Node) dialable() string {
	n.where.mu.Lock()
	defer n.where.mu.Unlock()
	return n.where.dialable
}

// findAddress works out where this node can be reached, when it hasn't
// lately.
func (n *Node) findAddress(ctx context.Context) {
	// Due every few minutes, and at once when the other nodes start seeing
	// this node at a different IP than it last found: a home connection's
	// IP changing is the usual case, and until the node re-registers,
	// requests to it (searches, for a node with a model) go to the old
	// address and fail.
	seen := n.seenIP()
	n.where.mu.Lock()
	due := time.Since(n.where.checked) >= addrCheckEvery ||
		(n.o.Advertise == "" && seen != "" && seen != n.where.publicIP)
	n.where.mu.Unlock()
	if !due {
		return
	}
	if n.o.Advertise != "" {
		n.setAddress(n.o.Advertise, "", true)
		return
	}
	ip := seen
	if ip == "" {
		if n.o.publicIP != nil {
			ip = n.o.publicIP(ctx) // tests
		} else {
			ip = n.whoamiViaDomains(ctx)
		}
	}
	if ip == "" {
		n.setAddress("", "", false)
		return
	}
	_, port, _ := net.SplitHostPort(n.o.Listen)
	candidate := net.JoinHostPort(ip, port)

	// Ask another node that can be reached to connect back. With none to
	// ask, trust the IP being on this machine's own interfaces.
	reachable, asked := false, false
	nodes, _ := n.st.Nodes()
	for _, nd := range nodes {
		if nd.ID == n.o.ID || nd.Addr == "" || ctx.Err() != nil {
			continue
		}
		asked = true
		var rep struct{ OK bool }
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := n.client.PostJSON(cctx, nd.Addr, "/sync/dialback", map[string]string{"addr": candidate}, &rep)
		cancel()
		if err == nil {
			reachable = rep.OK
			break
		}
	}
	if !asked {
		reachable = onInterface(ip)
	}
	addr := ""
	if reachable {
		addr = candidate
	}
	n.setAddress(addr, ip, reachable)
}

func (n *Node) setAddress(addr, ip string, reachable bool) {
	n.where.mu.Lock()
	changed := n.where.dialable != addr || n.where.checked.IsZero()
	n.where.dialable, n.where.publicIP, n.where.reachable = addr, ip, reachable
	n.where.checked = time.Now()
	n.where.mu.Unlock()
	if changed {
		if addr == "" {
			logf("other nodes can't reach this one (public IP %q): it will do the talking", ip)
		} else {
			logf("other nodes reach this one at %s", addr)
		}
	}
}

// seenIP is the public IP other nodes most recently saw this node's
// requests come from, if it's a public one (a node on the same private
// network sees a private address, which is no use to the rest).
func (n *Node) seenIP() string {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	var best string
	var at time.Time
	for _, p := range n.peers {
		if p.seen != "" && p.heard.After(at) && isPublic(p.seen) {
			best, at = p.seen, p.heard
		}
	}
	return best
}

// whoamiViaDomains finds this node's public IP by asking the node the
// site's domains lead to, on the cluster port, which address it sees this
// node's request come from.
func (n *Node) whoamiViaDomains(ctx context.Context) string {
	domains, err := n.st.Domains()
	if err != nil {
		return ""
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
			var rep whoami
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := n.client.GetJSON(cctx, net.JoinHostPort(ip, port), "/sync/whoami", &rep)
			cancel()
			if err == nil && net.ParseIP(rep.Seen) != nil {
				return rep.Seen
			}
		}
	}
	return ""
}

// whoami is /sync/whoami's answer: who answered, and the address the
// request was seen coming from.
type whoami struct {
	ID   string `json:"id"`
	Seen string `json:"seen"`
}

func isPublic(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.IsGlobalUnicast() && !p.IsPrivate()
}

// onInterface reports whether ip is one of this machine's own addresses.
func onInterface(ip string) bool {
	want := net.ParseIP(ip)
	addrs, err := net.InterfaceAddrs()
	if err != nil || want == nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(want) {
			return true
		}
	}
	return false
}

// peerKey carries the calling node's id (its certificate's name) in a
// request's context. The internal API's handlers see plain HTTP (see
// mux.go), so the TLS connection's details are picked up when it opens.
type peerKey struct{}

// callerID is the id of the node that made a request to the internal API,
// from its cluster certificate: something it can't claim falsely.
func callerID(r *http.Request) string {
	id, _ := r.Context().Value(peerKey{}).(string)
	return id
}

// connPeer is the http.Server ConnContext that records it.
func connPeer(ctx context.Context, c net.Conn) context.Context {
	if pc, ok := c.(plainConn); ok {
		if tc, ok := pc.Conn.(*tls.Conn); ok {
			if certs := tc.ConnectionState().PeerCertificates; len(certs) > 0 {
				return context.WithValue(ctx, peerKey{}, certs[0].Subject.CommonName)
			}
		}
	}
	return ctx
}

// remoteIP is the IP a request came from.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
