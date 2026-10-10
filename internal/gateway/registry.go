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

// registry tracks live tunnels. It is indexed three ways:
//   - bySecret: keyed by the reclaim credential, for sticky reconnect.
//   - byPublic: keyed by the public id (the thing in the URL), for routing
//     inbound agent requests.
//   - byRegistry: keyed by the registry id (the /v2/<rid>/ prefix of image
//     references), for routing image pulls.
//
// Routing entries (byPublic, byRegistry) are inserted at bind() — i.e. only once
// a tunnel actually exists — so a session in either map always has (or just
// had) a live yamux session; the reaper evicts ones whose tunnel has closed.
type registry struct {
	mu         sync.Mutex
	bySecret   map[string]*session
	byPublic   map[string]*session
	byRegistry map[string]*session
	publicBase string // e.g. "https://gw.example.com" or "http://gw.example.com" (no trailing slash)
	// registryHost is the host[:port] of the public URL when the gateway offers
	// FeatureRegistry (see registryHostFor), "" when it does not. Every reply
	// then carries the session's PublicRegistry (<registryHost>/<rid>); the
	// register handler clears it again for a connection that does not
	// negotiate the feature.
	registryHost string
	// grpcBase is the external base URL of the gateway's gRPC endpoint
	// (--public-grpc-url, no trailing slash). When set, every reply also carries
	// the session's Public GRPC URL (<grpcBase>/grpc/<publicID>) — the address a
	// remote `fleet` dials; fleetclient parses the trailing id back out as the
	// fleet-session routing key. Empty = no gRPC URL is minted.
	grpcBase string
	max      int
	signer   *tokenSigner // signs the session-resume token in every reply (see token.go)
}

// errAtCapacity is returned by claim when the session cap is reached. Since the
// gateway intentionally requires no registration auth, this cap is the basic
// guard against unbounded session creation.
var errAtCapacity = errors.New("gateway at capacity")

func newRegistry(publicBase, grpcBase string, max int, signer *tokenSigner) *registry {
	return &registry{
		bySecret:   make(map[string]*session),
		byPublic:   make(map[string]*session),
		byRegistry: make(map[string]*session),
		publicBase: publicBase,
		grpcBase:   grpcBase,
		max:        max,
		signer:     signer,
	}
}

// newID returns a 256-bit cryptographically-random lowercase hex id. Used for
// the secret reclaim credential, the public id and the registry id; 256 bits
// makes the public and registry ids unguessable (each is the capability that
// isolates a fleetd's traffic of its kind), and lowercase hex is a valid OCI
// repository path component.
func newID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// claim resolves a registration to a session and the reply to send back. On a
// reclaim (req.SessionID is a known secret, or req.SessionToken is a session
// token this gateway's key signed) it returns the session — live, or
// resurrected under its original ids — and its original public URL. Otherwise
// it RESERVES a fresh slot: it checks the cap and inserts the new session into
// bySecret atomically (under the registry lock), so the MaxSessions cap is a
// HARD limit even under a burst of concurrent first-time registrations. A
// reserved-or-resurrected session is not yet routable (byPublic is populated at
// bind); the caller must bind() on a successful handshake or release() the
// reservation on failure (isNew marks those).
func (r *registry) claim(req tunnel.RegisterRequest) (s *session, reply tunnel.RegisterReply, isNew bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.SessionID != "" {
		if existing, ok := r.bySecret[req.SessionID]; ok {
			return existing, r.reply(existing), false, nil
		}
		// Unknown secret (e.g. the gateway restarted, or a stale file): try the
		// signed session token below before minting a fresh session.
	}

	// Stable-URL path (issue #120): the secret is not in memory — typically
	// because the gateway restarted and lost its registry — but the request
	// carries a session token signed with this gateway's session key. The
	// signature proves a gateway holding the key minted these ids, so resurrect
	// the session under them and the daemon keeps its public URL (and its
	// registry id, so image references stay valid).
	if claims, ok := r.signer.verify(req.SessionToken); ok {
		if existing, ok := r.bySecret[claims.Secret]; ok {
			return existing, r.reply(existing), false, nil
		}
		if len(r.bySecret) >= r.max {
			return nil, tunnel.RegisterReply{}, false, errAtCapacity
		}
		_, publicTaken := r.byPublic[claims.PublicID]
		registryTaken := false
		if claims.RegistryID != "" {
			_, registryTaken = r.byRegistry[claims.RegistryID]
		}
		if !publicTaken && !registryTaken {
			registryID := claims.RegistryID
			if registryID == "" {
				// A token minted before the registry route: keep the public
				// id (MCP and webhook URLs stay put) and mint the rid now. The
				// reply's fresh token carries it from here on.
				if registryID, err = newID(); err != nil {
					return nil, tunnel.RegisterReply{}, false, err
				}
			}
			s = newSession(claims.Secret, claims.PublicID, registryID, r.publicBase)
			r.bySecret[claims.Secret] = s // reserve the slot, as for a fresh mint
			return s, r.reply(s), true, nil
		}
		// The public or registry id is already routed by a DIFFERENT session
		// (impossible for honestly-minted ids; defensive): fall through to a
		// fresh mint.
	}

	if len(r.bySecret) >= r.max {
		return nil, tunnel.RegisterReply{}, false, errAtCapacity
	}

	var secret, publicID, registryID string
	for _, id := range []*string{&secret, &publicID, &registryID} {
		if *id, err = newID(); err != nil {
			return nil, tunnel.RegisterReply{}, false, err
		}
	}
	s = newSession(secret, publicID, registryID, r.publicBase)
	r.bySecret[secret] = s // reserve the slot now -> cap is a hard limit
	return s, r.reply(s), true, nil
}

// reply builds the success RegisterReply for s. It mints a FRESH session token
// every time (claims and reclaims alike), so the daemon always ends up holding
// a token signed with the gateway's current key — a rotated key self-heals on
// the first successful reconnect.
//
// PublicGRPCURL is included whenever the gateway has a public gRPC base; the
// register handler clears it again when the connection does not negotiate the
// grpc feature, so an MCP-only daemon never sees a gRPC URL it can't serve.
//
// PublicWebhookURL is always computed from the public base — webhooks ride the
// gateway's public HTTP listener (the same one that serves /mcp), so no separate
// operator-configured base is needed (unlike gRPC's --public-grpc-url). The
// register handler clears it when the connection does not negotiate the webhook
// feature, so a daemon that didn't enable webhooks never sees a URL it can't serve.
//
// PublicRegistry is included whenever the gateway offers the registry feature
// (registryHost set), and cleared the same way for a connection that does not
// negotiate it.
func (r *registry) reply(s *session) tunnel.RegisterReply {
	reply := tunnel.RegisterReply{
		SessionID:        s.secret,
		PublicURL:        s.publicURL,
		PublicWebhookURL: r.publicBase + "/webhook/" + s.publicID,
		SessionToken:     r.signer.mint(s.secret, s.publicID, s.registryID),
	}
	if r.grpcBase != "" {
		reply.PublicGRPCURL = r.grpcBase + "/grpc/" + s.publicID
	}
	if r.registryHost != "" {
		reply.PublicRegistry = r.registryHost + "/" + s.registryID
	}
	return reply
}

// bind installs the live tunnel on a claimed session and makes it routable. It
// closes any tunnel it replaced (a reconnect supersedes the old, now-dead
// connection). bySecret already holds the session (reserved at claim); bind adds
// the byPublic and byRegistry routes after the tunnel is live.
func (r *registry) bind(s *session, ym *yamux.Session) {
	old := s.setYamux(ym)

	r.mu.Lock()
	r.byPublic[s.publicID] = s
	r.byRegistry[s.registryID] = s
	r.mu.Unlock()

	if old != nil && old != ym {
		_ = old.Close()
	}

	// The session's cached gRPC client conn may be sitting in connect backoff
	// armed against the old, dead tunnel (up to ~2 minutes after repeated failed
	// redials). That backoff must not outlive the tunnel it timed out against:
	// kick the conn so it redials the fresh tunnel immediately — setYamux above
	// already installed it, so the redial lands on the live session.
	s.resetGRPCBackoff()
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
	delete(r.byRegistry, s.registryID)
}

// lookup finds the session for a public id (nil if unknown).
func (r *registry) lookup(publicID string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byPublic[publicID]
}

// lookupRegistry finds the session for a registry id (nil if unknown).
func (r *registry) lookupRegistry(registryID string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byRegistry[registryID]
}

// attachTarget finds the session a registry attach connection names by its
// secret. Only a session that has been bound (it has, or just had, a main
// tunnel) is attachable: an attach never creates or resurrects a session, so a
// gateway that restarted waits for the daemon's main tunnel to reconnect first.
func (r *registry) attachTarget(secret string) *session {
	r.mu.Lock()
	s := r.bySecret[secret]
	r.mu.Unlock()
	if s == nil || s.currentYamux() == nil {
		return nil
	}
	return s
}

// attachRegistry installs ym as s's registry connection, superseding (and
// closing) an older one, and reports whether it stuck. The attach was checked
// against s before its handshake reply, so meanwhile s may have been reaped or
// have dropped the registry feature on a re-registration. Both close the
// registry connection they find, but ym may land after they looked, and would
// then stay bound to a dead or registry-less session. Checking again after the
// install closes that gap: either the check sees the change, or the change
// comes after the install and closes ym itself.
func (r *registry) attachRegistry(s *session, ym *yamux.Session) bool {
	if old := s.setRegistryYamux(ym); old != nil && old != ym {
		_ = old.Close()
	}
	if s.registry.Load() && r.lookupRegistry(s.registryID) == s {
		return true
	}
	s.clearRegistryYamux(ym)
	_ = ym.Close()
	return false
}

// reap evicts sessions whose tunnel has been closed past ttl. The grace window
// is what makes sticky reconnect work across a brief drop: a disconnected
// session keeps its URL reserved (its secret stays claimable) until ttl elapses
// with no reconnect. A reconnect within the window reclaims the same URL; only
// after the window is the URL freed.
func (r *registry) reap(now time.Time, ttl time.Duration) {
	r.mu.Lock()
	// Iterate bySecret (the superset): it holds bound sessions AND unbound
	// reservations, both of which reapable() can evict.
	var evicted []*session
	for secret, s := range r.bySecret {
		if s.reapable(now, ttl) {
			delete(r.bySecret, secret)
			delete(r.byPublic, s.publicID)
			delete(r.byRegistry, s.registryID)
			evicted = append(evicted, s)
		}
	}
	r.mu.Unlock()

	// Close each evicted session's cached gRPC client conn and its registry
	// connection (outside the registry lock). A reaped session object is never
	// resurrected — a reclaim past the TTL mints a NEW session — so an unclosed
	// conn would keep redialing the dead tunnel (and leak its goroutines)
	// forever, and a registry connection would outlive its session.
	for _, s := range evicted {
		s.closeGRPC()
		s.closeRegistry()
	}
}
