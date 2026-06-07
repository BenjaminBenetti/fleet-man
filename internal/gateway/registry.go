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
	publicBase string // e.g. "https://gw.example.com" (no trailing slash)
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
// reclaim (req.SessionID is a known secret) it returns the existing session and
// its original public URL. Otherwise it mints a fresh secret + public id. The
// caller must bind() the live yamux session afterward; claim does NOT insert a
// brand-new session into the routing maps (that happens at bind), so a half-open
// registration never becomes routable or counts against the reaper.
func (r *registry) claim(req tunnel.RegisterRequest) (*session, tunnel.RegisterReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.SessionID != "" {
		if s, ok := r.bySecret[req.SessionID]; ok {
			return s, tunnel.RegisterReply{SessionID: s.secret, PublicURL: s.publicURL}, nil
		}
		// Unknown secret (e.g. the gateway restarted, or a stale file): fall
		// through and mint a fresh session rather than failing.
	}

	if len(r.bySecret) >= r.max {
		return nil, tunnel.RegisterReply{}, errAtCapacity
	}

	secret, err := newID()
	if err != nil {
		return nil, tunnel.RegisterReply{}, err
	}
	publicID, err := newID()
	if err != nil {
		return nil, tunnel.RegisterReply{}, err
	}
	s := &session{
		secret:    secret,
		publicID:  publicID,
		publicURL: r.publicBase + "/mcp/" + publicID,
	}
	return s, tunnel.RegisterReply{SessionID: secret, PublicURL: s.publicURL}, nil
}

// bind installs the live tunnel on s and makes it routable. It closes any tunnel
// it replaced (a reconnect supersedes the old, now-dead connection). Setting the
// yamux before inserting into the maps means a session is only ever routable with
// a live tunnel attached.
func (r *registry) bind(s *session, ym *yamux.Session) {
	old := s.setYamux(ym)

	r.mu.Lock()
	r.bySecret[s.secret] = s
	r.byPublic[s.publicID] = s
	r.mu.Unlock()

	if old != nil && old != ym {
		_ = old.Close()
	}
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
	for id, s := range r.byPublic {
		if s.reapable(now, ttl) {
			delete(r.byPublic, id)
			delete(r.bySecret, s.secret)
		}
	}
}
