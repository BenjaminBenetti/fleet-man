package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
)

// registry.go is fleetd's side of gateway image delivery on the tunnel: the
// registry prefix the gateway assigned (Registry, WithRegistryChange) and the
// dedicated registry connection.
//
// Image layers would otherwise share the main tunnel with MCP, webhooks and
// remote gRPC and queue ahead of them, so pulls get a connection of their own:
// its own gRPC Register stream (and so its own TCP connection), whose
// handshake is RegisterRequest{Attach: "registry", SessionID: <secret>}, run
// with the registry yamux config (4 MiB stream window, 60s keepalive
// timeout). fleetd opens it on demand (WarmRegistry: a create, start or
// evacuation of a gateway-delivered instance, or a pull arriving on the main
// tunnel) and closes it after registryIdleTimeout without a stream. While none
// is attached the gateway sends pulls down the main tunnel, so pulls fleetd
// did not start still work.

// registryIdleTimeout closes an idle registry connection.
const registryIdleTimeout = 60 * time.Second

// attachSettle is how long an attach must last to count as having served:
// one that drops sooner (the gateway re-checks the session right after its
// reply) is retried with backoff.
const attachSettle = 2 * time.Second

// attachRetryMax caps the delay between attach attempts.
const attachRetryMax = 10 * time.Second

// unemitted is registryState.emitted before anything was emitted.
const unemitted = "\x00"

// registryState is the image registry as the Manager knows it.
type registryState struct {
	// gatewayURL is the gateway prefix and why are for.
	gatewayURL string
	// prefix is the gateway's PublicRegistry ("<host[:port]>/<rid>"), "" when
	// there is none, and why says why.
	prefix, why string
	// live: prefix came from a registration of this run (else the session
	// file's, while the tunnel connects).
	live bool
	// emitted is the prefix last passed to onRegistry.
	emitted string
	// warmAt is the last WarmRegistry.
	warmAt time.Time
	// attached reports a live registry connection; attaches counts them.
	attached bool
	attaches int
}

// Registry returns the image registry prefix the gateway assigned this
// daemon ("<host[:port]>/<rid>"; an image is <prefix>/devcontainer@<digest>),
// or "" and why there is none. While the tunnel reconnects, the last
// registration's prefix stays: the session, and its registry id, are
// reclaimed.
func (m *Manager) Registry() (prefix, why string) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	return m.reg.prefix, m.reg.why
}

// WarmRegistry asks for the registry connection: a pod is about to pull. It
// is opened (once the tunnel is registered with FeatureRegistry) and kept
// until it has been idle for registryIdleTimeout after the last call.
func (m *Manager) WarmRegistry() {
	m.regMu.Lock()
	m.reg.warmAt = time.Now()
	m.regMu.Unlock()
	select {
	case m.regWake <- struct{}{}:
	default:
	}
}

// setRegistry records the registry state and tells onRegistry of a changed
// prefix.
func (m *Manager) setRegistry(gatewayURL, prefix, why string, live bool) {
	m.regMu.Lock()
	m.reg.gatewayURL, m.reg.prefix, m.reg.why, m.reg.live = gatewayURL, prefix, why, live
	emit := m.onRegistry != nil && m.reg.emitted != prefix
	if emit {
		m.reg.emitted = prefix
	}
	m.regMu.Unlock()
	if emit {
		m.onRegistry(prefix)
	}
}

// Reasons Registry gives for having no prefix.
const (
	whyRegistryNotWired  = "image delivery through the gateway is not available in this daemon"
	whyRegistryOff       = "Enable Image Registry is off"
	whyRegistryOffered   = "the gateway does not offer image delivery (it predates it, runs without --session-key, or its --public-url is not https at the host root)"
	whyRegistryPending   = "the gateway tunnel is connecting"
	whyRegistryTunnelOff = "the gateway tunnel is off"
)

// registryConnecting updates the registry state as an attempt starts:
// without the feature wanted there is none; with it, a reconnect to the same
// gateway keeps the last registration's prefix, and a fresh start takes the
// session file's.
func (m *Manager) registryConnecting(d desiredState) {
	switch {
	case m.registryLis == nil:
		m.setRegistry(d.gatewayURL, "", whyRegistryNotWired, false)
	case !d.registry:
		m.setRegistry(d.gatewayURL, "", whyRegistryOff, false)
	default:
		m.regMu.Lock()
		keep := m.reg.gatewayURL == d.gatewayURL && m.reg.prefix != ""
		m.regMu.Unlock()
		if keep {
			return
		}
		if p := loadSession(d.gatewayURL).PublicRegistry; p != "" {
			m.setRegistry(d.gatewayURL, p, "", false)
		} else {
			m.setRegistry(d.gatewayURL, "", whyRegistryPending, false)
		}
	}
}

// registryNotNegotiated records a registration without FeatureRegistry.
func (m *Manager) registryNotNegotiated(d desiredState) {
	why := whyRegistryOffered
	switch {
	case m.registryLis == nil:
		why = whyRegistryNotWired
	case !d.registry:
		why = whyRegistryOff
	}
	m.setRegistry(d.gatewayURL, "", why, true)
}

func (m *Manager) registryIdleTimeout() time.Duration {
	if m.registryIdle > 0 {
		return m.registryIdle
	}
	return registryIdleTimeout
}

// registryWanted reports whether a WarmRegistry is recent enough to keep (or
// open) the registry connection.
func (m *Manager) registryWanted(now time.Time) bool {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	return !m.reg.warmAt.IsZero() && now.Sub(m.reg.warmAt) < m.registryIdleTimeout()
}

func (m *Manager) noteAttached(on bool) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	m.reg.attached = on
	if on {
		m.reg.attaches++
	}
}

// runRegistryConnection keeps the registry connection of one registered
// tunnel (secret is its session's) while it is wanted, until ctx ends with
// the tunnel. An attach the gateway refuses (right after a gateway restart
// it does not know the session until the main tunnel has re-bound) or that
// drops at once is retried with backoff.
func (m *Manager) runRegistryConnection(ctx context.Context, gatewayURL, secret string) {
	backoff := initialBackoff
	for {
		if !m.registryWanted(time.Now()) {
			select {
			case <-ctx.Done():
				return
			case <-m.regWake:
				continue
			}
		}
		served, err := m.attachRegistry(ctx, gatewayURL, secret)
		if ctx.Err() != nil {
			return
		}
		if served {
			backoff = initialBackoff
			continue
		}
		flog.Info("remote gateway registry connection failed; retrying", "gateway", gatewayURL, "err", err)
		t := time.NewTimer(jitter(backoff))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, attachRetryMax)
	}
}

// attachRegistry runs one registry connection until it closes: idle, dropped
// or ctx done. served reports whether it carried a stream or lasted past
// attachSettle.
func (m *Manager) attachRegistry(ctx context.Context, gatewayURL, secret string) (served bool, err error) {
	conn, err := m.dial(ctx, gatewayURL)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reply, err := tunnel.Handshake(conn, tunnel.RegisterRequest{
		Attach:        tunnel.AttachRegistry,
		SessionID:     secret,
		ClientVersion: m.clientVersion,
		Features:      []string{tunnel.FeatureRegistry},
	}, handshakeTimeout)
	if err != nil {
		return false, fmt.Errorf("registry attach handshake: %w", err)
	}
	if reply.Error != "" {
		return false, fmt.Errorf("gateway refused the registry connection: %s", reply.Error)
	}
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureRegistry) {
		return false, errors.New("the gateway did not take the registry connection")
	}
	if m.registryConnWrap != nil {
		conn = m.registryConnWrap(conn)
	}
	session, err := tunnel.RegistryClientSession(conn, m.logOut)
	if err != nil {
		return false, fmt.Errorf("yamux client (registry): %w", err)
	}
	defer session.Close()
	m.noteAttached(true)
	defer m.noteAttached(false)
	flog.Info("remote gateway registry connection attached", "gateway", gatewayURL)

	start := time.Now()
	idleDone := make(chan struct{})
	defer close(idleDone)
	go m.closeWhenIdle(session, idleDone)
	lis := tunnelListeners{registry: m.registryLis}
	streams := 0
	for {
		stream, aerr := session.Accept()
		if aerr != nil {
			err = aerr
			break
		}
		streams++
		go dispatchStream(stream, nil, lis)
	}
	served = streams > 0 || time.Since(start) >= attachSettle
	flog.Info("remote gateway registry connection closed", "gateway", gatewayURL, "streams", streams, "seconds", int(time.Since(start).Seconds()))
	return served, err
}

// closeWhenIdle closes session once it has had no stream for the idle
// timeout and nothing asked for it meanwhile.
func (m *Manager) closeWhenIdle(session *yamux.Session, done <-chan struct{}) {
	idle := m.registryIdleTimeout()
	tick := time.NewTicker(min(idle/4, 5*time.Second))
	defer tick.Stop()
	lastActive := time.Now()
	for {
		select {
		case <-done:
			return
		case <-session.CloseChan():
			return
		case now := <-tick.C:
			if session.NumStreams() > 0 {
				lastActive = now
				continue
			}
			if now.Sub(lastActive) >= idle && !m.registryWanted(now) {
				flog.Info("remote gateway registry connection idle; closing")
				_ = session.Close()
				return
			}
		}
	}
}
