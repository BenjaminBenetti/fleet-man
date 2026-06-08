package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
)

// registry tracks live tunnels. It is indexed two ways:
//   - bySecret: keyed by the reclaim credential, for sticky reconnect.
//   - byPublic: keyed by the public id (the thing in the URL), for routing
//     inbound agent requests.
//
// Entries are inserted at bind() — i.e. only once a tunnel actually exists — so a
// session in either map always has (or just had) a live yamux session; the
// reaper evicts ones whose tunnel has closed.
type registry struct {
	mu         sync.Mutex
	bySecret   map[string]*session
	byPublic   map[string]*session
	publicBase string // e.g. "https://gw.example.com" or "http://gw.example.com" (no trailing slash)
	max        int
}

// errAtCapacity is returned by claim when the session cap is reached. Since the
// gateway intentionally requires no registration auth, this cap is the basic
// guard against unbounded session creation.
var errAtCapacity = errors.New("gateway at capacity")

func newRegistry(publicBase string, max int) *registry {
	return &registry{
		bySecret:   make(map[string]*session),
		byPublic:   make(map[string]*session),
		publicBase: publicBase,
		max:        max,
	}
}

// newID returns a 256-bit cryptographically-random hex id. Used for both the
// secret reclaim credential and the public id; 256 bits makes the public id
// unguessable (it is the capability that isolates each fleetd).
func newID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// claim resolves a registration to a session and the reply to send back. On a
// reclaim (req.SessionID is a known secret) it returns the existing session
// (isNew=false) and its original public URL. Otherwise it RESERVES a fresh slot:
// it checks the cap and inserts the new session into bySecret atomically (under
// the registry lock), so the MaxSessions cap is a HARD limit even under a burst
// of concurrent first-time registrations. The reserved session is not yet
// routable (byPublic is populated at bind); the caller must bind() on a
// successful handshake or release() the reservation on failure.
func (r *registry) claim(req tunnel.RegisterRequest) (s *session, reply tunnel.RegisterReply, isNew bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.SessionID != "" {
		if existing, ok := r.bySecret[req.SessionID]; ok {
			return existing, tunnel.RegisterReply{SessionID: existing.secret, PublicURL: existing.publicURL}, false, nil
		}
		// Unknown secret (e.g. the gateway restarted, or a stale file): fall
		// through and mint a fresh session rather than failing.
	}

	if len(r.bySecret) >= r.max {
		return nil, tunnel.RegisterReply{}, false, errAtCapacity
	}

	secret, err := newID()
	if err != nil {
		return nil, tunnel.RegisterReply{}, false, err
	}
	publicID, err := newID()
	if err != nil {
		return nil, tunnel.RegisterReply{}, false, err
	}
	s = &session{
		secret:    secret,
		publicID:  publicID,
		publicURL: r.publicBase + "/mcp/" + publicID,
		createdAt: time.Now(),
	}
	r.bySecret[secret] = s // reserve the slot now -> cap is a hard limit
	return s, tunnel.RegisterReply{SessionID: secret, PublicURL: s.publicURL}, true, nil
}

// bind installs the live tunnel on a claimed session and makes it routable. It
// closes any tunnel it replaced (a reconnect supersedes the old, now-dead
// connection). bySecret already holds the session (reserved at claim); bind adds
// the byPublic route after the tunnel is live.
func (r *registry) bind(s *session, ym *yamux.Session) {
	old := s.setYamux(ym)

	r.mu.Lock()
	r.byPublic[s.publicID] = s
	r.mu.Unlock()

	if old != nil && old != ym {
		_ = old.Close()
	}
}

// release frees a reservation whose handshake failed before bind. It only removes
// a session that never got a live tunnel, so it is safe to call after a failed
// re-handshake of an already-bound session (it is a no-op in that case).
func (r *registry) release(s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.currentYamux() != nil {
		return // already bound (e.g. a reclaim) — keep it
	}
	delete(r.bySecret, s.secret)
	delete(r.byPublic, s.publicID)
}

// lookup finds the session for a public id (nil if unknown).
func (r *registry) lookup(publicID string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byPublic[publicID]
}

// reap evicts sessions whose tunnel has been closed past ttl. The grace window
// is what makes sticky reconnect work across a brief drop: a disconnected
// session keeps its URL reserved (its secret stays claimable) until ttl elapses
// with no reconnect. A reconnect within the window reclaims the same URL; only
// after the window is the URL freed.
func (r *registry) reap(now time.Time, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Iterate bySecret (the superset): it holds bound sessions AND unbound
	// reservations, both of which reapable() can evict.
	for secret, s := range r.bySecret {
		if s.reapable(now, ttl) {
			delete(r.bySecret, secret)
			delete(r.byPublic, s.publicID)
		}
	}
}
