// Package gateway is the fleet gateway: a standalone, operator-run, remote-hosted
// server that relays remote MCP traffic to a user's fleetd over a reverse tunnel.
// It is run via `fleet gateway` and is the public, internet-facing entrypoint of
// the remote-MCP feature.
//
// It is deliberately ISOLATED from the fleet daemon: it imports only the shared
// wire protocol (internal/tunnel) and the standard library — never internal/server,
// internal/state, internal/flog, or any other fleetd internal. The gateway has no
// access to ~/.fleet and holds no fleet state; it only routes bytes.
//
// # Two listeners
//
//   - Control (TLS): fleetd dials in here and performs the internal/tunnel
//     handshake, then the connection becomes a yamux session the gateway opens
//     streams on. This is NOT an HTTP endpoint.
//   - Public (HTTPS): remote MCP agents connect here at https://<host>/mcp/<id>.
//     Each request is reverse-proxied down the matching tunnel.
//
// # Security
//
// By design there is NO registration auth: anyone may open a tunnel. Isolation
// comes from the unguessable 256-bit public id in the URL, and the real access
// boundary remains the MCP bearer token, which the gateway forwards untouched to
// fleetd's loopback MCP server. The reclaim credential (the tunnel "secret") is
// kept out of the public URL so URL holders cannot hijack a tunnel.
package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	defaultControlAddr = ":8443"
	defaultPublicAddr  = ":443"
	defaultMaxSessions = 1024

	// controlHandshakeTimeout bounds the pre-yamux register exchange so a stalled
	// or non-fleetd client can't tie up a control conn indefinitely.
	controlHandshakeTimeout = 15 * time.Second
	// reapInterval is how often closed tunnels are swept from the registry.
	reapInterval = 15 * time.Second
	// sessionTTL is how long a disconnected session keeps its URL reserved so a
	// reconnecting fleetd (with its secret) recovers the same URL. After this
	// elapses with no reconnect, the reaper frees the URL.
	sessionTTL = 10 * time.Minute
	// maxPendingHandshakes is the headroom (beyond MaxSessions) of in-flight
	// control connections allowed at once. Because the gateway requires no auth,
	// this bounds a slow-accept flood: once the cap is hit, new control
	// connections are shed (closed) rather than each spawning a goroutine that
	// lingers for the handshake timeout.
	maxPendingHandshakes = 256
	// reservationGrace bounds how long a claimed-but-never-bound session slot may
	// linger before the reaper frees it. Longer than controlHandshakeTimeout so an
	// in-progress handshake is never reaped; a backstop to explicit release.
	reservationGrace = 2 * controlHandshakeTimeout
	// shutdownTimeout bounds the public server drain on shutdown.
	shutdownTimeout = 5 * time.Second
)

// Config configures a gateway. PublicURL, TLSCert, and TLSKey are required.
type Config struct {
	ControlAddr string // address fleetd dials in on (TLS). Default ":8443".
	PublicAddr  string // address agents hit (HTTPS). Default ":443".
	PublicURL   string // external base URL agents use, e.g. "https://gw.example.com". Required.
	TLSCert     string // path to the TLS certificate (PEM). Required.
	TLSKey      string // path to the TLS private key (PEM). Required.
	MaxSessions int    // cap on concurrent tunnels. Default 1024.
}

// Server is a configured gateway. Build with New, then Run.
type Server struct {
	cfg       Config
	reg       *registry
	tlsConfig *tls.Config
	log       *slog.Logger
}

// New validates cfg, loads the TLS keypair, and returns a ready Server.
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.PublicURL) == "" {
		return nil, errors.New("gateway: --public-url is required (e.g. https://gw.example.com)")
	}
	if cfg.TLSCert == "" || cfg.TLSKey == "" {
		return nil, errors.New("gateway: --tls-cert and --tls-key are required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("gateway: load TLS keypair: %w", err)
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = defaultControlAddr
	}
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = defaultPublicAddr
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")

	return &Server{
		cfg:       cfg,
		reg:       newRegistry(cfg.PublicURL, cfg.MaxSessions),
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		log:       slog.Default(),
	}, nil
}

// Serve is the convenience entrypoint used by the CLI: New + Run.
func Serve(ctx context.Context, cfg Config) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// Run binds both listeners (up front, so a bind error surfaces immediately) and
// serves until ctx is cancelled (SIGINT/SIGTERM) or a listener fails fatally.
func (s *Server) Run(ctx context.Context) error {
	controlLn, err := tls.Listen("tcp", s.cfg.ControlAddr, s.tlsConfig)
	if err != nil {
		return fmt.Errorf("gateway: listen control %s: %w", s.cfg.ControlAddr, err)
	}
	publicLn, err := tls.Listen("tcp", s.cfg.PublicAddr, s.tlsConfig)
	if err != nil {
		_ = controlLn.Close()
		return fmt.Errorf("gateway: listen public %s: %w", s.cfg.PublicAddr, err)
	}
	return s.ServeListeners(ctx, controlLn, publicLn)
}

// ServeListeners runs the accept loops, reaper, and public HTTP server over
// already-bound listeners until ctx is cancelled or the public server fails. Run
// is the usual entrypoint (it binds from Config); this is exposed for embedding
// the gateway on caller-supplied listeners (e.g. socket activation) and for
// integration tests that need ephemeral ports. The listeners must already carry
// TLS (Run wraps them with the configured cert).
func (s *Server) ServeListeners(ctx context.Context, controlLn, publicLn net.Listener) error {
	defer controlLn.Close()

	publicSrv := &http.Server{
		Handler:           s.publicHandler(),
		ReadHeaderTimeout: 10 * time.Second, // bound header reads (slowloris); body/SSE unbounded
	}

	// Bound concurrent control goroutines (established sessions + in-flight
	// handshakes) so the no-auth control port can't be flooded into unbounded
	// goroutine growth. Sized so MaxSessions established tunnels plus a handshake
	// headroom always fit; excess connections are shed in serveControl.
	controlSem := make(chan struct{}, s.cfg.MaxSessions+maxPendingHandshakes)
	go s.serveControl(ctx, controlLn, controlSem)
	go s.runReaper(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- publicSrv.Serve(publicLn) }()

	s.log.Info("fleet gateway started",
		"control", s.cfg.ControlAddr, "public", s.cfg.PublicAddr, "public_url", s.cfg.PublicURL)

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = publicSrv.Shutdown(sctx)
		_ = controlLn.Close()
		s.log.Info("fleet gateway stopped")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("gateway: public server: %w", err)
	}
}

// runReaper periodically sweeps closed tunnels from the registry.
func (s *Server) runReaper(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reg.reap(time.Now(), sessionTTL)
		}
	}
}

// yamuxLog returns the writer yamux logs its internals to. Discarded — the
// gateway logs the events it cares about (register/close) itself.
func (s *Server) yamuxLog() io.Writer { return io.Discard }
