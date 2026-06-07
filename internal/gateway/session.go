package gateway

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// errNoTunnel is returned by session.open when the session has no live tunnel
// (e.g. fleetd is mid-reconnect). The public handler turns it into a 502.
var errNoTunnel = errors.New("gateway: session has no live tunnel")

// session is one registered fleetd: the reverse tunnel back to it plus its two
// identifiers.
//
// CRITICAL split: `secret` is the reclaim credential — fleetd persists it and
// sends it on reconnect to recover the same URL — and it is NEVER placed in the
// public URL. `publicID` is what appears in publicURL and is therefore visible
// to anyone the agent shares the URL with. Keeping them distinct means a holder
// of the public URL cannot re-register (hijack) the tunnel, because that
// requires the secret, which they never see.
type session struct {
	secret    string
	publicID  string
	publicURL string

	mu sync.Mutex
	ym *yamux.Session // current live tunnel; replaced on reconnect (older one closed)
	// closedAt is when the current tunnel was first observed closed (zero while
	// live). The session — and its public URL — is RESERVED for a grace TTL after
	// this, so a fleetd that drops and reconnects (with its secret) recovers the
	// same URL. The reaper frees it only once the TTL elapses with no reconnect.
	closedAt time.Time
}

// setYamux installs ym as the live tunnel and returns the one it replaced (nil on
// first bind). Installing a tunnel clears closedAt, so a reconnect un-expires the
// session.
func (s *session) setYamux(ym *yamux.Session) (old *yamux.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, s.ym, s.closedAt = s.ym, ym, time.Time{}
	return old
}

// currentYamux returns the live tunnel (nil if none / not yet bound).
func (s *session) currentYamux() *yamux.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ym
}

// reapable reports whether the session should be evicted: its tunnel is closed
// AND has stayed closed past ttl. It lazily stamps closedAt on the first
// observation of a closed tunnel, so the grace clock starts even if nothing else
// noticed the disconnect.
func (s *session) reapable(now time.Time, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ym == nil {
		return true // never bound (defensive; bound sessions always have a tunnel)
	}
	if !s.ym.IsClosed() {
		return false
	}
	if s.closedAt.IsZero() {
		s.closedAt = now
		return false
	}
	return now.Sub(s.closedAt) > ttl
}

// open dials a fresh multiplexed stream to fleetd over the live tunnel. Each
// inbound public request gets its own stream, so SSE and concurrent requests
// never interleave.
func (s *session) open() (net.Conn, error) {
	s.mu.Lock()
	ym := s.ym
	s.mu.Unlock()
	if ym == nil {
		return nil, errNoTunnel
	}
	return ym.Open()
}
