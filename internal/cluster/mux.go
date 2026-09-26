package cluster

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

// One port carries all node-to-node traffic: a small internal HTTP API
// ("RPC" below) over mutual TLS. Replication (sync.go), submitting a
// write to a node that holds its file, fetching photos, and passing on
// web requests for a group this node doesn't hold all use it.
//
// The listener still picks the protocol by TLS ALPN (the name a client
// offers during the handshake), as it did when Raft shared the port, so a
// future protocol can be added beside the API without a second port.
const rpcProto = "grus-rpc"

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
	// Agree to our protocol when the client asks for it; anything else
	// completes the handshake and route then hangs up.
	sconf.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		for _, p := range hello.SupportedProtos {
			if p == rpcProto {
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
		c.Close() // a protocol nobody here speaks
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
// Closing it stops only that protocol, not the port.
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

// hostAddr is a host:port that isn't resolved until it's dialed.
type hostAddr string

func (a hostAddr) Network() string { return "tcp" }
func (a hostAddr) String() string  { return string(a) }
