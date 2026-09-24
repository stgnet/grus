package cluster

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/stgnet/grus/internal/cmd"
)

// One port carries all node-to-node traffic:
//
//   - Raft's own protocol (log replication, votes, snapshots) for each log
//     this node follows: site.db's, and one per group it holds, and
//   - a small internal HTTP API ("RPC" below): submitting commands to a
//     log's leader, fetching photos, passing on web requests for a group
//     this node doesn't hold.
//
// They're told apart by TLS ALPN, the protocol name a client offers during
// the handshake: an HTTP client asks for rpcProto, and a Raft connection
// for one log asks for raftProto(log), "grus-raft/site" or "grus-raft/g42".
// So there's one port to open and one certificate check for everything,
// and each log's Raft instance only ever sees its own connections.
const (
	rpcProto        = "grus-rpc"
	raftProtoPrefix = "grus-raft/"
)

func raftProto(log cmd.LogID) string { return raftProtoPrefix + log.String() }

// muxListener accepts TLS connections on the cluster port and hands each to
// the listener registered for its protocol.
type muxListener struct {
	ln   net.Listener
	mu   sync.Mutex
	subs map[string]*subListener // by ALPN protocol
	done chan struct{}
	once sync.Once
}

func newMux(listen string, conf *tls.Config) (*muxListener, error) {
	sconf := conf.Clone()
	sconf.NextProtos = nil
	// A TLS server normally has a fixed list of protocols it speaks, but
	// ours depends on which logs this node holds right now, which changes
	// as groups are placed. So pick per connection: agree to whichever of
	// our protocols the client asked for. (An unknown log still completes
	// the handshake; route then hangs up, as nobody's listening for it.)
	sconf.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		for _, p := range hello.SupportedProtos {
			if p == rpcProto || strings.HasPrefix(p, raftProtoPrefix) {
				c := conf.Clone()
				c.NextProtos = []string{p}
				return c, nil
			}
		}
		return nil, nil // no protocol we know: route closes it
	}
	ln, err := tls.Listen("tcp", listen, sconf)
	if err != nil {
		return nil, err
	}
	m := &muxListener{ln: ln, subs: map[string]*subListener{}, done: make(chan struct{})}
	go m.acceptLoop()
	return m, nil
}

func (m *muxListener) acceptLoop() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			m.Close()
			return
		}
		go m.route(c.(*tls.Conn))
	}
}

// route finishes the handshake (with a deadline, so a silent client can't
// hold a goroutine forever) and passes the connection on.
func (m *muxListener) route(c *tls.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	proto := c.ConnectionState().NegotiatedProtocol
	m.mu.Lock()
	sub := m.subs[proto]
	m.mu.Unlock()
	if sub == nil {
		// A log this node doesn't follow (yet, or any more). The other
		// side's Raft retries, which is right: it may be a moment early.
		c.Close()
		return
	}
	conn := net.Conn(c)
	if proto == rpcProto {
		// net/http treats a *tls.Conn with an ALPN protocol it doesn't
		// know as "someone else's protocol" and hangs up. Hiding the TLS
		// type makes it serve plain HTTP/1.1 over the already-secured
		// connection, which is what we want.
		conn = plainConn{c}
	}
	select {
	case sub.ch <- conn:
	case <-sub.done:
		c.Close()
	case <-m.done:
		c.Close()
	}
}

// listen registers a listener for one protocol.
func (m *muxListener) listen(proto string, addr net.Addr) *subListener {
	l := &subListener{m: m, proto: proto, ch: make(chan net.Conn), done: make(chan struct{}), addr: addr}
	m.mu.Lock()
	m.subs[proto] = l
	m.mu.Unlock()
	return l
}

func (m *muxListener) Close() error {
	var err error
	m.once.Do(func() {
		close(m.done)
		err = m.ln.Close()
	})
	return err
}

// plainConn is a connection with its TLS type hidden (see route).
type plainConn struct{ net.Conn }

// subListener is one protocol's side of the mux, as a net.Listener.
// Closing it stops only that protocol (one log's Raft leaving this node),
// not the port.
type subListener struct {
	m     *muxListener
	proto string
	ch    chan net.Conn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

var errClosed = errors.New("listener closed")

func (l *subListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, errClosed
	case <-l.m.done:
		return nil, errClosed
	}
}

func (l *subListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.m.mu.Lock()
		if l.m.subs[l.proto] == l {
			delete(l.m.subs, l.proto)
		}
		l.m.mu.Unlock()
	})
	return nil
}

func (l *subListener) Addr() net.Addr { return l.addr }

// tlsStream is one log's Raft network layer over mutual TLS: its side of
// the mux for incoming connections, and a Dial that asks the far end for
// the same log.
type tlsStream struct {
	*subListener
	conf *tls.Config
}

func newTLSStream(m *muxListener, log cmd.LogID, advertise string, conf *tls.Config) *tlsStream {
	proto := raftProto(log)
	dconf := conf.Clone()
	dconf.NextProtos = []string{proto}
	return &tlsStream{subListener: m.listen(proto, hostAddr(advertise)), conf: dconf}
}

// Dial connects to another node. The address is host:port and the host is
// resolved on every dial, so changing a node's DNS record is enough to move
// it.
func (s *tlsStream) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(d, "tcp", string(addr), s.conf)
}

// hostAddr is a host:port that isn't resolved until it's dialed.
type hostAddr string

func (a hostAddr) Network() string { return "tcp" }
func (a hostAddr) String() string  { return string(a) }
