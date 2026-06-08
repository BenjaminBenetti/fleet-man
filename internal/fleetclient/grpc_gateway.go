package fleetclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// grpc_gateway.go reaches a daemon's gRPC server THROUGH a fleet gateway, so a
// remote `fleet` client can drive a daemon it cannot reach directly. The gateway
// exposes a NATIVE gRPC endpoint (h2c, or h2 under TLS); this dials it as an
// ordinary gRPC target — TLS verified against the OS system roots for https, or
// plaintext h2c for http (TLS terminated upstream by a reverse proxy). Every RPC
// carries two pieces of metadata: the daemon's session id as `fleet-session` (the
// gateway reads it to route to the right tunnel) and the MCP bearer token as
// `authorization` (the daemon validates it). The gateway only routes; the token
// stays the real access boundary.
//
// This stays inside the fleetclient import boundary (stdlib + grpc only).

// grpcSessionHeader is the gRPC metadata key carrying the target session id. It
// MUST match the gateway's grpcSessionHeader (internal/gateway/grpc.go); the
// import boundary forbids sharing the constant.
const grpcSessionHeader = "fleet-session"

// gatewayEndpoint reaches a daemon through a fleet gateway. Built (and validated)
// by newGatewayEndpoint from a FLEET_GATEWAY URL.
type gatewayEndpoint struct {
	rawURL string // original FLEET_GATEWAY (carries no secret), for String()
	target string // gRPC dial target, e.g. "dns:///gw.example.com:443"
	useTLS bool   // https -> verified TLS; http -> plaintext h2c
	id     string // the daemon's session id (the fleet-session routing key)
	token  string // MCP bearer token
}

// newGatewayEndpoint parses a FLEET_GATEWAY URL of the form
// <scheme>://<host[:port]>/<id> (a trailing /grpc/<id> is also accepted). https
// dials verified TLS; http dials plaintext h2c. The port defaults to 443 (https)
// or 80 (http) when omitted.
func newGatewayEndpoint(rawURL, token string) (gatewayEndpoint, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return gatewayEndpoint{}, fmt.Errorf("parse FLEET_GATEWAY: %w", err)
	}
	var useTLS bool
	var defaultPort string
	switch u.Scheme {
	case "https":
		useTLS, defaultPort = true, "443"
	case "http":
		useTLS, defaultPort = false, "80"
	default:
		return gatewayEndpoint{}, fmt.Errorf("FLEET_GATEWAY must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return gatewayEndpoint{}, fmt.Errorf("FLEET_GATEWAY has no host: %q", rawURL)
	}
	id := lastPathSegment(u.Path)
	if id == "" {
		return gatewayEndpoint{}, fmt.Errorf("FLEET_GATEWAY must include the session id, e.g. https://gw.example.com:50051/<id>")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	return gatewayEndpoint{
		rawURL: rawURL,
		target: "dns:///" + net.JoinHostPort(u.Hostname(), port),
		useTLS: useTLS,
		id:     id,
		token:  token,
	}, nil
}

// lastPathSegment returns the final non-empty path segment, so both "/<id>" and
// "/grpc/<id>" yield "<id>".
func lastPathSegment(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Target is the gRPC dial target (the gateway's host:port).
func (e gatewayEndpoint) Target() string { return e.target }

// IsLocal is false — no auto-spawn / version-restart for a remote daemon.
func (e gatewayEndpoint) IsLocal() bool { return false }

// String is the gateway URL (which carries no secret).
func (e gatewayEndpoint) String() string { return e.rawURL }

// DialOptions wires the transport credentials (verified TLS for https, plaintext
// h2c for http) plus per-RPC metadata: the fleet-session routing id (read by the
// gateway) and the bearer token (validated by the daemon).
func (e gatewayEndpoint) DialOptions() []grpc.DialOption {
	var creds credentials.TransportCredentials
	if e.useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		creds = insecure.NewCredentials()
	}
	return []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(gatewayPerRPC{token: e.token, sessionID: e.id}),
	}
}

// gatewayPerRPC attaches the routing id and bearer token to every RPC.
// RequireTransportSecurity is false so the metadata also rides the plaintext h2c
// transport (the token's exposure is the same gateway-trust model as before).
type gatewayPerRPC struct {
	token     string
	sessionID string
}

func (g gatewayPerRPC) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{
		"authorization":   "Bearer " + g.token,
		grpcSessionHeader: g.sessionID,
	}, nil
}

func (g gatewayPerRPC) RequireTransportSecurity() bool { return false }
