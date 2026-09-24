package cluster

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// One port carries two kinds of node-to-node traffic:
//
//   - Raft's own protocol (log replication, votes, snapshots), and
//   - a small internal HTTP API ("RPC" below): fetching photo blobs, and
//     submitting commands to the leader from tools like import-archive.
//
// They're told apart by TLS ALPN, the protocol name a client offers during
// the handshake: an HTTP client asks for rpcProto; Raft's dialer asks for
// nothing. So there's one port to open and one certificate check for both,
// and nothing else to configure.
const rpcProto = "grus-rpc"

// muxListener accepts TLS connections on the cluster port and hands each to
// Raft or to the RPC server.
type muxListener struct {
	ln   net.Listener
	raft chan net.Conn
	rpc  chan net.Conn
	done chan struct{}
	once sync.Once
}

func newMux(listen string, conf *tls.Config) (*muxListener, error) {
	sconf := conf.Clone()
	sconf.NextProtos = []string{rpcProto}
	ln, err := tls.Listen("tcp", listen, sconf)
	if err != nil {
		return nil, err
	}
	m := &muxListener{ln: ln, raft: make(chan net.Conn), rpc: make(chan net.Conn), done: make(chan struct{})}
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
	ch, conn := m.raft, net.Conn(c)
	if c.ConnectionState().NegotiatedProtocol == rpcProto {
		// net/http treats a *tls.Conn with an ALPN protocol it doesn't
		// know as "someone else's protocol" and hangs up. Hiding the TLS
		// type makes it serve plain HTTP/1.1 over the already-secured
		// connection, which is what we want.
		ch, conn = m.rpc, plainConn{c}
	}
	select {
	case ch <- conn:
	case <-m.done:
		c.Close()
	}
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

// subListener is one side of the mux, as a net.Listener.
type subListener struct {
	m    *muxListener
	ch   chan net.Conn
	addr net.Addr
}

var errClosed = errors.New("listener closed")

func (l *subListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.m.done:
		return nil, errClosed
	}
}

func (l *subListener) Close() error   { return l.m.Close() }
func (l *subListener) Addr() net.Addr { return l.addr }

// tlsStream is Raft's network layer over mutual TLS: the Raft side of the
// mux for incoming connections, and a Dial for outgoing ones.
type tlsStream struct {
	*subListener
	conf *tls.Config
}

func newTLSStream(m *muxListener, advertise string, conf *tls.Config) *tlsStream {
	dconf := conf.Clone()
	dconf.NextProtos = nil // no ALPN: that's how the far end knows it's Raft
	return &tlsStream{subListener: &subListener{m: m, ch: m.raft, addr: hostAddr(advertise)}, conf: dconf}
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
