// Package remote drives fleetd's side of the remote-MCP reverse tunnel: it dials
// the user-configured fleet gateway, registers (sticky session id), and serves
// the gateway's inbound MCP requests by reverse-proxying them to the loopback MCP
// server. It is server-only (imported solely by internal/server) and additive —
// the local MCP server in mcp.go is untouched; tunneled requests hit the same
// loopback listener and the same bearer-token auth as local clients.
//
// The Manager is a single supervisor goroutine. Config changes arrive via
// Reconcile (non-blocking, safe to call under the service's config lock) and the
// loop reacts: connect when enabled, tear down when disabled, reconnect on drop
// with jittered exponential backoff. Every state transition is published as a
// fleetgrpc.RemoteMcpStatus so the TUI's settings page reflects it live.
package remote

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

const (
	// initialBackoff and maxBackoff bound the reconnect schedule. Full jitter is
	// applied per sleep, and the schedule resets after an established connection.
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 60 * time.Second
	// handshakeTimeout bounds the pre-yamux control exchange so a silent gateway
	// can't wedge an attempt forever. yamux manages its own timeouts after.
	handshakeTimeout = 15 * time.Second
)

// desiredState is the latest config the manager should converge to. mcp, grpc
// and webhook are the three independent exposure toggles ("Enable Remote MCP" /
// "Enable Remote Fleet" / "Enable Webhook"); the tunnel connects while ANY is on,
// and each gates its own traffic kind on that shared tunnel.
type desiredState struct {
	mcp        bool
	grpc       bool
	webhook    bool
	gatewayURL string
}

// on reports whether the tunnel should be up at all. It delegates to
// state.RemoteMcpSettings.TunnelDesired so the dial decision here and the TUI's
// remote-connection indicator share one definition and can't drift.
func (d desiredState) on() bool {
	return state.RemoteMcpSettings{
		Enabled:        d.mcp,
		FleetEnabled:   d.grpc,
		WebhookEnabled: d.webhook,
		GatewayURL:     d.gatewayURL,
	}.TunnelDesired()
}

// Manager supervises the outbound tunnel. Construct with NewManager and drive
// with Run (once) + Reconcile (on every config change).
type Manager struct {
	mcpPort       int
	clientVersion string
	publish       func(*fleetgrpc.RemoteMcpStatus)
	logOut        io.Writer

	// dial is the transport seam: in production it TLS-dials an https gateway
	// (verified against the system roots) or plaintext-dials an http one; tests
	// override it to reach an in-process gateway.
	dial func(ctx context.Context, gatewayURL string) (net.Conn, error)

	// grpcLis, when set, enables tunneling the daemon's gRPC server alongside MCP:
	// the Manager advertises FeatureGRPC, and on a gateway that negotiates it,
	// demuxes tagged streams (grpc streams are pushed here). nil = MCP only.
	grpcLis *ChanListener

	// webhookLis, when set, enables tunneling the daemon's automation webhook
	// receiver: the Manager advertises FeatureWebhook, and on a gateway that
	// negotiates it, demuxes TagWebhook streams to this listener. nil = no webhook.
	webhookLis *ChanListener

	mu            sync.Mutex
	desired       desiredState
	attemptCancel context.CancelFunc // cancels the in-flight connect attempt, if any
	wake          chan struct{}      // size-1 nudge: Reconcile wakes the loop
}

// Option customizes a Manager. The production daemon uses none; the transport
// seam (WithDialFunc) exists for tests and for future custom transports.
type Option func(*Manager)

// WithDialFunc overrides how the Manager opens the control connection to the
// gateway (the default dials TLS for an https gateway URL, verified against the
// system roots, or plaintext TCP for an http one). Used by integration tests to
// reach an in-process gateway with a test CA.
func WithDialFunc(dial func(ctx context.Context, gatewayURL string) (net.Conn, error)) Option {
	return func(m *Manager) { m.dial = dial }
}

// WithGRPCListener enables tunneling the daemon's gRPC server alongside MCP. lis
// is fed demuxed gRPC streams and is Served by a gRPC server in the daemon (with
// the bearer-token auth interceptor). Without this option the Manager tunnels
// only MCP.
func WithGRPCListener(lis *ChanListener) Option {
	return func(m *Manager) { m.grpcLis = lis }
}

// WithWebhookListener enables tunneling the daemon's automation webhook receiver.
// lis is fed demuxed TagWebhook streams and is Served by an http.Server in the
// daemon (the webhook receiver — unauthenticated, since the unguessable public
// URL is the capability). Without this option the Manager tunnels no webhooks.
func WithWebhookListener(lis *ChanListener) Option {
	return func(m *Manager) { m.webhookLis = lis }
}

// features lists the tunnel capabilities this Manager requests in its
// RegisterRequest for the attempt's desired state. Each feature is advertised
// only when its listener is wired AND the user enabled that traffic kind — NOT
// negotiating a feature is what makes the matching gateway endpoint dead for this
// session (the gateway withholds its URL and 404s/​rejects requests for it), so a
// disabled daemon never sees that traffic at all.
func (m *Manager) features(d desiredState) []string {
	var f []string
	if m.grpcLis != nil && d.grpc {
		f = append(f, tunnel.FeatureGRPC)
	}
	if m.webhookLis != nil && d.webhook {
		f = append(f, tunnel.FeatureWebhook)
	}
	return f
}

// NewManager builds a Manager that reverse-proxies to the loopback MCP server on
// mcpPort and reports status via publish (which must be safe to call from the
// manager's goroutine — typically a hub.post). A zero mcpPort means the local
// MCP server is not running, in which case the manager reports an error while
// enabled rather than connecting.
func NewManager(mcpPort int, clientVersion string, publish func(*fleetgrpc.RemoteMcpStatus), opts ...Option) *Manager {
	m := &Manager{
		mcpPort:       mcpPort,
		clientVersion: clientVersion,
		publish:       publish,
		logOut:        io.Discard,
		dial:          dialGateway,
		wake:          make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Reconcile records the desired config and nudges the supervisor. mcpEnabled,
// fleetEnabled and webhookEnabled are the three exposure toggles (remote MCP /
// remote fleet control / automation webhooks). It is NON-BLOCKING: it sets state,
// cancels any in-flight attempt so it re-evaluates, and signals the loop — so it
// is safe to call while holding other locks (e.g. the service's config-write lock
// in SetConfig).
//
// An UNCHANGED desired state is a strict no-op. SetConfig calls Reconcile on
// EVERY config save, and most saves don't touch the remote settings — those
// must not bounce an established tunnel, because for a remote client (TUI over
// FLEET_GATEWAY) that tunnel carries the SetConfig RPC's own reply: cancelling
// the attempt would tear down the yamux session mid-RPC and the client would
// see its successful save fail with an Unavailable/EOF.
func (m *Manager) Reconcile(mcpEnabled, fleetEnabled, webhookEnabled bool, gatewayURL string) {
	next := desiredState{mcp: mcpEnabled, grpc: fleetEnabled, webhook: webhookEnabled, gatewayURL: strings.TrimSpace(gatewayURL)}
	m.mu.Lock()
	if next == m.desired {
		m.mu.Unlock()
		return // no change — leave the current attempt (and its tunnel) alone
	}
	m.desired = next
	cancel := m.attemptCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel() // interrupt the current attempt so it picks up the new desired state
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run is the supervisor loop. It blocks until ctx is cancelled (daemon
// shutdown), so callers start it on a goroutine. Exactly one attempt is ever in
// flight, so there is no reconnection storm.
func (m *Manager) Run(ctx context.Context) {
	backoff := initialBackoff
	// prev is the last desired config recorded in the event log, so config
	// transitions (enable/disable/URL change) log once — not per attempt of the
	// steady-state reconnect loop.
	var prev desiredState
	for {
		if ctx.Err() != nil {
			return
		}

		// Read the desired config AND install this attempt's cancel under ONE
		// lock. They must be atomic: if they were separate, a Reconcile landing
		// between them (the window is widened by the publish() below) would see no
		// cancel installed and let the attempt run on with stale settings — even
		// serving after a disable. With one lock, a Reconcile either precedes it
		// (we read its fresh desired) or follows it (it sees our cancel and aborts
		// this attempt).
		attemptCtx, cancel := context.WithCancel(ctx)
		m.mu.Lock()
		d := m.desired
		m.attemptCancel = cancel
		m.mu.Unlock()

		if d != prev {
			logDesiredChange(prev, d)
			prev = d
		}

		if !d.on() {
			cancel()
			m.mu.Lock()
			m.attemptCancel = nil
			m.mu.Unlock()
			m.publish(statusDisabled())
			if !m.wait(ctx) {
				return
			}
			backoff = initialBackoff
			continue
		}

		m.publish(statusConnecting())
		registered, err := m.connectAndServe(attemptCtx, d)
		cancel()

		m.mu.Lock()
		m.attemptCancel = nil
		changed := m.desired != d
		m.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		if changed {
			// Desired config changed mid-attempt (Reconcile). Re-evaluate now
			// with no backoff and no spurious error.
			backoff = initialBackoff
			continue
		}
		if registered {
			// An established tunnel dropped (gateway restart / network). Reconnect
			// promptly; the next iteration republishes CONNECTING.
			flog.Warn("remote gateway tunnel dropped; reconnecting", "gateway", d.gatewayURL, "err", err)
			backoff = initialBackoff
		} else {
			// Never connected this attempt — surface the failure while we back off.
			flog.Warn("remote gateway connect failed", "gateway", d.gatewayURL, "err", err)
			m.publish(statusError(err))
		}
		if !m.sleep(ctx, jitter(backoff)) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// connectAndServe runs one full attempt: dial, register, then serve the tunnel
// until it drops or attemptCtx is cancelled. registered reports whether the
// gateway accepted the registration (used to reset the backoff for an
// established-then-dropped connection).
func (m *Manager) connectAndServe(ctx context.Context, d desiredState) (registered bool, err error) {
	gatewayURL := d.gatewayURL
	// The whole remote surface depends on the local MCP stack: the tunnel comes up
	// only when MCP is up (its loopback port is the tunnel's anchor, and the
	// tunnel-facing gRPC server's bearer token IS the MCP token), so with
	// mcpPort == 0 even a remote-fleet- or webhook-only tunnel could serve nothing.
	// Fail the attempt — an explicit error in the settings page beats a
	// "connected" tunnel that silently serves no traffic kind.
	if m.mcpPort == 0 {
		return false, fmt.Errorf("local MCP server is not running (required for remote MCP, remote fleet, and webhooks)")
	}

	conn, err := m.dial(ctx, gatewayURL)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	// Close the conn on ctx cancel so the attempt aborts promptly at ANY stage:
	// the handshake (which tunnel.Handshake bounds with its own timeout) as well as
	// the serve loop (closing the conn under yamux makes serveProxy return).
	// Idempotent with the defers and with session.Close below.
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopOnCancel()

	// Register handshake over the stream (before yamux), bounded by its own timeout
	// (the StreamConn has no deadlines) so a silent gateway can't hang the attempt.
	prev := loadSession(gatewayURL)
	req := tunnel.RegisterRequest{
		SessionID:     prev.SessionID,
		SessionToken:  prev.SessionToken,
		ClientVersion: m.clientVersion,
		Features:      m.features(d),
	}
	reply, err := tunnel.Handshake(conn, req, handshakeTimeout)
	if err != nil {
		return false, fmt.Errorf("register handshake: %w", err)
	}
	if reply.Error != "" {
		return false, fmt.Errorf("gateway refused registration: %s", reply.Error)
	}

	// Registered. Persist the sticky session (id + resume token) and report the
	// public URLs.
	registered = true
	_ = saveSession(sessionFile{
		SessionID:        reply.SessionID,
		SessionToken:     reply.SessionToken,
		PublicURL:        reply.PublicURL,
		PublicGRPCURL:    reply.PublicGRPCURL,
		PublicWebhookURL: reply.PublicWebhookURL,
		GatewayURL:       gatewayURL,
	})
	flog.Info("remote gateway connected", "gateway", gatewayURL, "publicURL", reply.PublicURL, "publicGrpcURL", reply.PublicGRPCURL, "publicWebhookURL", reply.PublicWebhookURL, "gatewayVersion", reply.GatewayVersion)
	m.publish(statusConnected(reply.PublicURL, reply.PublicGRPCURL, reply.PublicWebhookURL, reply.GatewayVersion))

	session, err := tunnel.ClientSession(conn, m.logOut)
	if err != nil {
		return registered, fmt.Errorf("yamux client: %w", err)
	}
	defer session.Close()
	// The serve loop unblocks when the session/conn closes — on a peer drop, or
	// when stopOnCancel closes the conn on ctx cancel (shutdown / Reconcile
	// teardown). Use the demuxing path when the gateway negotiated gRPC and/or
	// webhook (each only requested when its toggle is on AND its listener is
	// wired); pass nil for a NON-negotiated kind so its streams are rejected.
	// Otherwise the streams are untagged (legacy MCP wire). Either path serves a
	// traffic kind only while its toggle is on — a toggle flip mid-connection
	// lands here again via Reconcile's attempt cancel + reconnect.
	grpcLis := m.grpcLis
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		grpcLis = nil
	}
	webhookLis := m.webhookLis
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureWebhook) {
		webhookLis = nil
	}
	if grpcLis != nil || webhookLis != nil {
		return registered, serveTunnel(session, m.mcpPort, grpcLis, webhookLis, d.mcp)
	}
	if !d.mcp {
		// Remote-fleet/webhook-only, but the gateway negotiated no tagging feature
		// (old gateway). Nothing may be served: park on the session, closing every
		// stream the gateway pushes, until it drops or the attempt is cancelled.
		return registered, serveReject(session)
	}
	return registered, serveProxy(session, m.mcpPort)
}

// logDesiredChange records a remote-gateway config transition in the event log:
// enabled, reconfigured (URL or toggle change while on), or disabled. A change
// that never turns the tunnel on (e.g. enabled with an empty URL) logs nothing.
func logDesiredChange(prev, d desiredState) {
	switch {
	case d.on() && !prev.on():
		flog.Info("remote gateway enabled", "gateway", d.gatewayURL, "mcp", d.mcp, "fleet", d.grpc)
	case d.on() && prev.on():
		flog.Info("remote gateway reconfigured", "gateway", d.gatewayURL, "mcp", d.mcp, "fleet", d.grpc)
	case prev.on():
		flog.Info("remote gateway disabled")
	}
}

// wait blocks until a Reconcile nudge arrives or ctx is cancelled. It returns
// false only on cancellation.
func (m *Manager) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-m.wake:
		return true
	}
}

// sleep waits for d, a Reconcile nudge, or cancellation. It returns false only
// on cancellation (a nudge or timeout both mean "proceed").
func (m *Manager) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-m.wake:
		return true
	case <-t.C:
		return true
	}
}

// nextBackoff doubles d up to maxBackoff.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// jitter applies full jitter: a uniformly random duration in [0, d]. Spreads out
// reconnect attempts so many fleetds don't thunder a recovering gateway.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
