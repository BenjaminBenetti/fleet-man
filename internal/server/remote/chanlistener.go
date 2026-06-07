package remote

import (
	"net"
	"sync"
)

// chanlistener.go provides an in-memory net.Listener whose connections are PUSHED
// in rather than accepted from an OS socket. The tunnel demux (serveTunnel)
// classifies each yamux stream by its tag and pushes it into the right listener:
// one feeds the loopback MCP http.Server, one feeds the daemon's tunnel-facing
// gRPC server. Neither served server needs a real port or socket.

// chanAddr is the placeholder address a ChanListener reports (used only for the
// served server's logging/diagnostics).
type chanAddr struct{}

func (chanAddr) Network() string { return "fleet-tunnel" }
func (chanAddr) String() string  { return "fleet-tunnel" }

// ChanListener is a net.Listener fed by Push. It satisfies net.Listener so a
// stdlib http.Server or a grpc.Server can Serve it.
type ChanListener struct {
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

// NewChanListener returns an open ChanListener.
func NewChanListener() *ChanListener {
	return &ChanListener{
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
}

// Push hands c to a waiting Accept. If the listener is already closed it closes c
// instead, so a late stream is never leaked.
func (l *ChanListener) Push(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

// Accept blocks until a connection is pushed or the listener is closed (after
// which it returns net.ErrClosed, which http.Server/grpc.Server treat as a clean
// stop).
func (l *ChanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops the listener; pending/future Accepts return net.ErrClosed and
// pushed connections are closed. Idempotent.
func (l *ChanListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

// Addr reports the placeholder tunnel address.
func (l *ChanListener) Addr() net.Addr { return chanAddr{} }
