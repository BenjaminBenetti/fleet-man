package fleetclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"

	"google.golang.org/grpc"
)

// grpc_gateway.go dials the daemon's gRPC server THROUGH a fleet gateway, so a
// remote `fleet` client can drive a daemon it cannot reach directly. The gateway
// exposes a hijack+splice endpoint at <gw>/grpc/<id>; the dialer connects to the
// gateway (TLS for an https URL, verified against the system roots; plaintext for
// an http URL behind a TLS-terminating proxy), performs the CONNECT-style
// handshake, and hands gRPC the resulting raw conn to run native HTTP/2 over.
// Every RPC carries the MCP bearer token as metadata, which the daemon validates.
//
// This stays inside the fleetclient import boundary (stdlib + grpc only).

// gatewayEndpoint reaches the daemon through a fleet gateway. rawURL is the public
// gRPC URL the gateway minted (FLEET_GATEWAY); token is the MCP bearer token.
type gatewayEndpoint struct {
	rawURL string
	token  string
}

// Target is a dummy: the real address lives in the CONNECT dialer, but gRPC
// requires a non-empty target.
func (e gatewayEndpoint) Target() string { return "passthrough:///fleet-gateway" }

// IsLocal is false — no auto-spawn / version-restart for a remote daemon.
func (e gatewayEndpoint) IsLocal() bool { return false }

// String is the gateway URL (which carries no secret).
func (e gatewayEndpoint) String() string { return e.rawURL }

// DialOptions wires the CONNECT dialer + the per-RPC bearer token. The inner
// transport is insecure because any TLS terminates at (or before) the gateway —
// the dialer establishes it for https, or it is plaintext for http; the gRPC
// connection then runs h2c over the tunneled conn.
func (e gatewayEndpoint) DialOptions() []grpc.DialOption {
	return append(insecureCreds(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialGatewayConn(ctx, e.rawURL)
		}),
		grpc.WithPerRPCCredentials(bearerPerRPC{token: e.token}),
	)
}

// dialGatewayConn dials the gateway (TLS for an https URL, verified against the
// system roots; plaintext TCP for an http URL) and performs the /grpc/<id>
// handshake: it sends a plain request to the path and expects "200 Connection
// Established", after which the connection is a transparent byte pipe to the
// daemon's gRPC server. The returned conn preserves any bytes the handshake
// reader buffered.
func dialGatewayConn(ctx context.Context, rawURL string) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse FLEET_GATEWAY: %w", err)
	}
	// Reject an authority-less URL up front (mirroring the control dialer in
	// internal/server/remote). Without this, an https URL with no host would have
	// an empty hostname and fall through to the plaintext branch below — silently
	// downgrading a URL the user wrote as https.
	if u.Hostname() == "" {
		return nil, fmt.Errorf("FLEET_GATEWAY has no host: %q", rawURL)
	}
	// serverName is the host for https (TLS SNI) and empty for http (plaintext).
	var serverName, defaultPort string
	switch u.Scheme {
	case "https":
		serverName, defaultPort = u.Hostname(), "443"
	case "http":
		serverName, defaultPort = "", "80"
	default:
		return nil, fmt.Errorf("FLEET_GATEWAY must be http or https, got %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("FLEET_GATEWAY must include the /grpc/<id> path")
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), defaultPort)
	}

	var conn net.Conn
	if serverName != "" {
		d := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
		conn, err = d.DialContext(ctx, "tcp", host)
	} else {
		var d net.Dialer
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("dial gateway %s: %w", host, err)
	}
	if c, err := gatewayHandshake(conn, host, u.Path); err != nil {
		_ = conn.Close()
		return nil, err
	} else {
		return c, nil
	}
}

// gatewayHandshake sends the tunnel request and reads the 200 response, returning
// a conn that yields any post-header buffered bytes before the live stream.
func gatewayHandshake(conn net.Conn, host, path string) (net.Conn, error) {
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, host); err != nil {
		return nil, fmt.Errorf("gateway handshake write: %w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("gateway handshake read: %w", err)
	}
	if !strings.Contains(status, " 200 ") {
		return nil, fmt.Errorf("gateway did not establish the tunnel (%s) — is the gateway up to date and the session live?", strings.TrimSpace(status))
	}
	for { // consume to the end of the response headers
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("gateway handshake headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// Reads go through br (draining any buffered bytes first); writes go straight
	// to the conn.
	return bufConn{Conn: conn, r: br}, nil
}

// bufConn is a net.Conn whose Read drains a bufio.Reader (so handshake-buffered
// bytes are not lost) before reading the underlying conn.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// bearerPerRPC attaches `authorization: Bearer <token>` to every RPC.
type bearerPerRPC struct{ token string }

func (b bearerPerRPC) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

// RequireTransportSecurity is false: TLS terminates at the gateway, so the inner
// gRPC transport is "insecure" from gRPC's point of view.
func (b bearerPerRPC) RequireTransportSecurity() bool { return false }
