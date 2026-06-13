// Package gateway is the fleet gateway: a standalone, operator-run, remote-hosted
// server that relays remote MCP traffic to a user's fleetd over a reverse tunnel.
// It is run via `fleet gateway` and is the public, internet-facing entrypoint of
// the remote-MCP feature.
//
// It is deliberately ISOLATED from the fleet daemon: it imports only the shared
// wire protocol (internal/tunnel), the standard library, and google.golang.org/grpc
// (to transparently proxy gRPC) — never internal/server, internal/state,
// internal/flog, or any other fleetd internal. The gateway has no access to
// ~/.fleet and holds no fleet state; it only routes bytes.
//
// # Two listeners
//
//   - Public (proxy.go): remote MCP agents connect here (HTTP/1.1) at
//     <public-url>/mcp/<id>. Each request is reverse-proxied down the matching
//     tunnel.
//   - gRPC (grpc.go + register.go): native gRPC over HTTP/2, serving TWO things —
//     remote `fleet` control RPCs (routed by the fleet-session metadata header and
//     proxied down the tunnel) AND fleetd registration (the Register bidi method,
//     which carries the reverse tunnel itself; see register.go). There is no
//     separate TCP control port — registration rides this HTTP/2 endpoint, so the
//     whole gateway is frontable by an L7 proxy.
//
// Both serve TLS when a cert+key are configured, or plain HTTP/h2c otherwise — the
// latter for deployment behind a TLS-terminating reverse proxy (e.g.
// Kubernetes/Traefik), which is then responsible for the public TLS.
//
// # Security
//
// By design there is NO registration auth: anyone may open a tunnel. Isolation
// comes from the unguessable 256-bit public id in the URL, and the real access
// boundary remains the MCP bearer token, which the gateway forwards untouched to
// fleetd's loopback MCP server. The reclaim credential (the tunnel "secret") is
// kept out of the public URL so URL holders cannot hijack a tunnel. Every
// registration also returns a session-resume token — a JWT over the session's
// ids signed with the gateway's session key (--session-key) — which fleetd
// presents on reconnect so its session URL stays stable even across gateway
// restarts (see token.go).
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

	"google.golang.org/grpc"
)

const (
	defaultPublicAddr  = ":443"
	defaultMaxSessions = 1024

	// registerHandshakeTimeout bounds the pre-yamux register exchange (on the gRPC
	// Register stream) so a stalled or non-fleetd client can't tie up a stream
	// indefinitely.
	registerHandshakeTimeout = 15 * time.Second
	// reapInterval is how often closed tunnels are swept from the registry.
	reapInterval = 15 * time.Second
	// sessionTTL is how long a disconnected session keeps its URL reserved so a
	// reconnecting fleetd (with its secret) recovers the same URL. After this
	// elapses with no reconnect, the reaper frees the URL.
	sessionTTL = 10 * time.Minute
	// reservationGrace bounds how long a claimed-but-never-bound session slot may
	// linger before the reaper frees it. Longer than registerHandshakeTimeout so an
	// in-progress handshake is never reaped; a backstop to explicit release.
	reservationGrace = 2 * registerHandshakeTimeout
	// shutdownTimeout bounds the public server drain on shutdown.
	shutdownTimeout = 5 * time.Second

	// maxPendingHandshakes is the headroom (beyond MaxSessions) of in-flight Register
	// streams allowed at once. Registration is UNAUTHENTICATED, so this bounds a
	// flood: once MaxSessions+maxPendingHandshakes handler goroutines are live, new
	// Register streams are shed (ResourceExhausted) instead of each lingering for the
	// handshake timeout. Replaces the old control-port load-shedding accept loop.
	maxPendingHandshakes = 256
	// maxConcurrentStreams caps streams per HTTP/2 connection on the gRPC server, so
	// a single connection can't open unbounded Register/RPC streams (defense in depth
	// alongside the global pending-handshake semaphore).
	maxConcurrentStreams = 256
)

// Config configures a gateway. PublicURL is required. TLSCert and TLSKey are
// optional: provide BOTH to serve HTTPS/TLS on the listeners, or NEITHER to
// serve plain HTTP (e.g. behind a TLS-terminating reverse proxy like Traefik).
type Config struct {
	PublicAddr    string // address MCP agents hit (HTTPS or HTTP). Default ":443".
	GRPCAddr      string // address the native-gRPC (h2c/h2) listener binds; also where fleetd registers. Empty disables remote gRPC + registration. CLI default ":50051".
	PublicURL     string // external base URL agents use, e.g. "https://gw.example.com" or "http://gw.example.com". Required.
	PublicGRPCURL string // external base URL remote `fleet` clients dial for the gRPC endpoint, e.g. "https://gw.example.com:50051". Empty = no Public GRPC URL is handed to daemons.
	TLSCert       string // path to the TLS certificate (PEM). Optional; both TLSCert and TLSKey together enable TLS.
	TLSKey        string // path to the TLS private key (PEM). Optional; both TLSCert and TLSKey together enable TLS.
	MaxSessions   int    // cap on concurrent tunnels. Default 1024.
	SessionKey    string // secret key signing session-resume tokens (token.go). Empty = a random per-boot key, so session URLs do not survive a gateway restart.
	Version       string // gateway build version, echoed to fleetd in the register reply for diagnostics. Empty = unset (dev build).
}

// Server is a configured gateway. Build with New, then Run. tlsConfig is nil when
// TLS is disabled (no cert/key) — the listeners are then plain TCP.
type Server struct {
	cfg       Config
	reg       *registry
	tlsConfig *tls.Config
	log       *slog.Logger
	// pendingSem bounds concurrent Register handler goroutines (in-flight handshakes
	// + established tunnels) on the unauthenticated registration surface. nil = no
	// cap (used by some in-process tests).
	pendingSem chan struct{}
}

// New validates cfg, loads the TLS keypair when one is configured, and returns a
// ready Server. TLS is optional: supply both --tls-cert and --tls-key to serve
// HTTPS, or neither to serve plain HTTP (for a TLS-terminating reverse proxy).
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.PublicURL) == "" {
		return nil, errors.New("gateway: --public-url is required (e.g. https://gw.example.com or http://gw.example.com)")
	}
	// TLS is all-or-nothing: a lone cert or key is a misconfiguration.
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, errors.New("gateway: --tls-cert and --tls-key must be provided together (or both omitted for plain HTTP)")
	}
	var tlsConfig *tls.Config
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("gateway: load TLS keypair: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = defaultPublicAddr
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	cfg.PublicGRPCURL = strings.TrimRight(strings.TrimSpace(cfg.PublicGRPCURL), "/")

	signer, err := newTokenSigner(cfg.SessionKey)
	if err != nil {
		return nil, fmt.Errorf("gateway: session token key: %w", err)
	}

	return &Server{
		cfg:        cfg,
		reg:        newRegistry(cfg.PublicURL, cfg.PublicGRPCURL, cfg.MaxSessions, signer),
		tlsConfig:  tlsConfig,
		log:        slog.Default(),
		pendingSem: make(chan struct{}, cfg.MaxSessions+maxPendingHandshakes),
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

// Run binds the listeners (up front, so a bind error surfaces immediately) and
// serves until ctx is cancelled (SIGINT/SIGTERM) or a listener fails fatally.
func (s *Server) Run(ctx context.Context) error {
	publicLn, err := s.listen(s.cfg.PublicAddr)
	if err != nil {
		return fmt.Errorf("gateway: listen public %s: %w", s.cfg.PublicAddr, err)
	}
	// The gRPC listener is always a PLAIN TCP listener — serveGRPC adds TLS itself
	// via grpc.Creds (so HTTP/2 ALPN is negotiated) or serves h2c when cert-less.
	// It hosts BOTH remote-control gRPC and fleetd registration (the Register bidi
	// stream + reverse tunnel). Empty GRPCAddr disables remote gRPC + registration.
	var grpcLn net.Listener
	if s.cfg.GRPCAddr != "" {
		grpcLn, err = net.Listen("tcp", s.cfg.GRPCAddr)
		if err != nil {
			_ = publicLn.Close()
			return fmt.Errorf("gateway: listen grpc %s: %w", s.cfg.GRPCAddr, err)
		}
	}
	return s.ServeListeners(ctx, publicLn, grpcLn)
}

// listen binds addr, wrapping it in TLS when a cert is configured and using a
// plain TCP listener otherwise (TLS terminated upstream by a reverse proxy).
func (s *Server) listen(addr string) (net.Listener, error) {
	if s.tlsConfig != nil {
		return tls.Listen("tcp", addr, s.tlsConfig)
	}
	return net.Listen("tcp", addr)
}

// ServeListeners runs the reaper and the public + gRPC servers over already-bound
// listeners until ctx is cancelled or a server fails. Run is the usual entrypoint
// (it binds from Config); this is exposed for embedding the gateway on
// caller-supplied listeners (e.g. socket activation) and for integration tests
// that need ephemeral ports. publicLn already carries its transport (TLS when a
// cert is configured, else plain TCP); grpcLn must be a PLAIN listener (serveGRPC
// adds TLS via grpc.Creds, or serves h2c) and may be nil to disable gRPC +
// registration. fleetd registration runs as the Register method on the gRPC server.
func (s *Server) ServeListeners(ctx context.Context, publicLn, grpcLn net.Listener) error {
	publicSrv := &http.Server{
		Handler:           s.publicHandler(),
		ReadHeaderTimeout: 10 * time.Second, // bound header reads (slowloris); body/SSE unbounded
	}

	go s.runReaper(ctx)

	// errCh collects fatal serve errors from the public and (optional) gRPC
	// servers; either failing tears the gateway down.
	errCh := make(chan error, 2)
	go func() { errCh <- publicSrv.Serve(publicLn) }()

	var grpcSrv *grpc.Server
	if grpcLn != nil {
		grpcSrv = s.newGRPCServer()
		go func() { errCh <- s.serveGRPC(grpcSrv, grpcLn) }()
	}

	s.log.Info("fleet gateway started",
		"public", s.cfg.PublicAddr, "grpc", s.cfg.GRPCAddr, "public_url", s.cfg.PublicURL)
	if s.cfg.SessionKey == "" {
		s.log.Warn("gateway: no --session-key set; session URLs will not survive a gateway restart")
	}

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = publicSrv.Shutdown(sctx)
		if grpcSrv != nil {
			grpcSrv.Stop()
		}
		s.log.Info("fleet gateway stopped")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("gateway: server: %w", err)
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
