package gateway

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
)

// errNoTunnel is returned by session.open when the session has no live tunnel
// (e.g. fleetd is mid-reconnect). The public handler turns it into a 502.
var errNoTunnel = errors.New("gateway: session has no live tunnel")

// session is one registered fleetd: the reverse tunnel back to it plus its
// three identifiers.
//
// CRITICAL split: `secret` is the reclaim credential — fleetd persists it and
// sends it on reconnect to recover the same URL — and it is NEVER placed in the
// public URL. `publicID` is what appears in publicURL and is therefore visible
// to anyone the agent shares the URL with. Keeping them distinct means a holder
// of the public URL cannot re-register (hijack) the tunnel, because that
// requires the secret, which they never see.
//
// `registryID` (rid) is a third 256-bit id: the path prefix of the session's
// image registry (/v2/<rid>/...). Image references are readable by anyone who
// can read pods, so it is never the public id, which the webhook URL uses as its
// only credential.
type session struct {
	secret     string
	publicID   string
	publicURL  string
	registryID string

	// createdAt is when the session slot was reserved (at claim). Used by the
	// reaper to evict a reservation whose handshake never completed (never bound).
	createdAt time.Time

	// grpc reports whether THIS connection negotiated FeatureGRPC. When set, the
	// gateway tags every stream it opens (TagMCP / TagGRPC) and serves the
	// /grpc/<id> route; when clear (legacy fleetd), streams are untagged and only
	// MCP is served. Atomic because it is set on the control goroutine and read on
	// public-request goroutines, and re-set on reconnect.
	grpc atomic.Bool

	// webhook reports whether THIS connection negotiated FeatureWebhook. When set,
	// the gateway serves the /webhook/<id>/<name> route and tags every stream it
	// opens (so fleetd can demux). Like grpc it forces the tagged-stream wire, so
	// either feature alone switches the tunnel into tag-demux mode. Atomic for the
	// same cross-goroutine reasons as grpc.
	webhook atomic.Bool

	// registry reports whether THIS connection negotiated FeatureRegistry. When
	// set, the gateway serves the /v2/<rid>/... route and accepts a registry
	// attach connection for the session. It forces the tagged-stream wire too.
	registry atomic.Bool

	// tagged reports whether THIS connection negotiated any tagging feature
	// (tunnel.Tagged), i.e. whether every stream opened on the main tunnel
	// starts with a tag byte. Set from the negotiated set with the same
	// predicate fleetd uses to demux, so the two ends never disagree.
	tagged atomic.Bool

	// registrySlots bounds the session's concurrent registry streams
	// (maxRegistryStreams), over the registry connection and the main tunnel
	// alike: each holds up to a tunnel.RegistryStreamWindow of buffered layer
	// bytes. A request past the cap waits for a slot.
	registrySlots chan struct{}

	mu sync.Mutex
	ym *yamux.Session // current live tunnel; nil until bind; replaced on reconnect
	// registryYm is the session's registry attach connection
	// (tunnel.AttachRegistry), nil while none is bound. It never replaces ym:
	// registry streams go over it while it is live and over ym otherwise.
	registryYm *yamux.Session
	// closedAt is when the current tunnel was first observed closed (zero while
	// live). The session — and its public URL — is RESERVED for a grace TTL after
	// this, so a fleetd that drops and reconnects (with its secret) recovers the
	// same URL. The reaper frees it only once the TTL elapses with no reconnect.
	closedAt time.Time
	// grpcCC is the lazily-built gRPC client connection the gRPC proxy uses to reach
	// the daemon's grpc.Server over this session's tunnel (see grpcClientConn). Built
	// once on first gRPC request; its custom dialer re-opens a TagGRPC stream on the
	// live tunnel, so it survives reconnects. Guarded by mu — NOT a sync.Once — so
	// bind() can read it to reset its connect backoff after a reconnect (see
	// resetGRPCBackoff) without racing the lazy build.
	grpcCC *grpc.ClientConn
}

// maxRegistryStreams caps a session's concurrent registry streams, so a daemon
// can have at most maxRegistryStreams × tunnel.RegistryStreamWindow (32 MiB) of
// layer bytes buffered in the gateway.
const maxRegistryStreams = 8

// newSession builds a session for its three ids, not yet bound to a tunnel.
func newSession(secret, publicID, registryID, publicBase string) *session {
	return &session{
		secret:        secret,
		publicID:      publicID,
		publicURL:     publicBase + "/mcp/" + publicID,
		registryID:    registryID,
		createdAt:     time.Now(),
		registrySlots: make(chan struct{}, maxRegistryStreams),
	}
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

// reapable reports whether the session should be evicted. A bound session is
// reaped once its tunnel has stayed closed past ttl (lazily stamping closedAt on
// first observation, so the grace clock starts even if nothing else noticed the
// disconnect). An UNBOUND reservation — claimed but whose handshake never reached
// bind — is reaped once it is older than reservationGrace; this is only a backstop
// (handshake failures release their reservation explicitly), but it guarantees a
// slot can never be reserved forever.
func (s *session) reapable(now time.Time, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ym == nil {
		return now.Sub(s.createdAt) > reservationGrace
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
func (s *session) open(tag byte) (net.Conn, error) {
	s.mu.Lock()
	ym := s.ym
	s.mu.Unlock()
	if ym == nil {
		return nil, errNoTunnel
	}
	conn, err := ym.Open()
	if err != nil {
		return nil, err
	}
	// When this connection negotiated a tagging feature (gRPC, webhook or
	// registry), fleetd demuxes streams by a leading tag byte, so the gateway
	// must write it as the first bytes of every stream. Legacy (un-negotiated)
	// sessions get untagged streams (the old MCP wire).
	if s.tagged.Load() {
		if err := tunnel.WriteTag(conn, tag); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// openRegistry dials a TagRegistry stream for one registry request: over the
// registry attach connection while one is bound and live, else over the main
// tunnel. The fallback keeps pulls fleetd did not start (an eviction, kubelet
// image GC) working while fleetd has no registry connection open.
func (s *session) openRegistry() (net.Conn, error) {
	if rym := s.currentRegistryYamux(); rym != nil && !rym.IsClosed() {
		conn, err := rym.Open()
		if err == nil {
			// A registry connection carries only registry streams, and is
			// always tagged (tunnel.AttachRegistry).
			if err = tunnel.WriteTag(conn, tunnel.TagRegistry); err == nil {
				return conn, nil
			}
			_ = conn.Close()
		}
		// The registry connection died under us: use the main tunnel.
	}
	return s.open(tunnel.TagRegistry)
}

// setRegistryYamux installs ym as the session's registry connection and returns
// the one it replaced (nil if none). A newer attach supersedes an older one, so
// a daemon whose registry connection went half-open is not locked out until the
// gateway notices.
func (s *session) setRegistryYamux(ym *yamux.Session) (old *yamux.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, s.registryYm = s.registryYm, ym
	return old
}

// clearRegistryYamux empties the registry slot if ym still holds it (a newer
// attach may already have replaced it).
func (s *session) clearRegistryYamux(ym *yamux.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryYm == ym {
		s.registryYm = nil
	}
}

// currentRegistryYamux returns the registry connection (nil if none is bound).
func (s *session) currentRegistryYamux() *yamux.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registryYm
}

// closeRegistry closes and clears the session's registry connection, if any:
// when the session is reaped, or when a re-registration no longer negotiates
// the registry feature.
func (s *session) closeRegistry() {
	if old := s.setRegistryYamux(nil); old != nil {
		_ = old.Close()
	}
}
