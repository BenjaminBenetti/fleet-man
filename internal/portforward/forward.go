package portforward

import (
	"fmt"
	"net"
	"sync"
)

// Forward represents a single active port forward binding.
type Forward struct {
	LocalPort    int  `json:"local_port"`
	RemotePort   int  `json:"remote_port"`
	BrowserProxy bool `json:"browser_proxy"` // true when this forward serves a browser proxy

	listener net.Listener
	done     chan struct{} // closed when the accept loop stops
	conns    *connSet      // live accepted connections (nil on List copies)
}

// Label returns a display string like "8080->80".
// Browser proxy forwards are labelled "PORT->proxy" to distinguish
// them from regular port forwards.
func (f Forward) Label() string {
	if f.BrowserProxy {
		return fmt.Sprintf("%d->proxy", f.LocalPort)
	}
	return fmt.Sprintf("%d->%d", f.LocalPort, f.RemotePort)
}

// stopForward cleanly shuts down a forward: stop accepting, then close the
// live local connections — their proxy pumps end and tear down the
// per-connection gRPC streams (and the server-side bridges with them).
func stopForward(fwd *Forward) {
	if fwd.listener != nil {
		fwd.listener.Close()
		if fwd.done != nil {
			<-fwd.done
		}
	}
	if fwd.conns != nil {
		fwd.conns.closeAll()
	}
}

// connSet tracks a forward's live accepted connections. A pointer field on
// Forward (rather than an embedded mutex) so Forward stays copyable for
// List/ListAll snapshots.
type connSet struct {
	mu sync.Mutex
	m  map[net.Conn]struct{}
}

func newConnSet() *connSet {
	return &connSet{m: make(map[net.Conn]struct{})}
}

func (s *connSet) add(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[c] = struct{}{}
}

func (s *connSet) remove(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, c)
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.m {
		_ = c.Close()
	}
}
