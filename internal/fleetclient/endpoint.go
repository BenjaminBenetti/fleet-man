// Package fleetclient is the client side of the fleet client/server split: it
// dials the fleet server, auto-spawns a local one when needed, runs the version
// handshake, and hands callers a fleetgrpc.FleetServiceClient.
//
// BOUNDARY: this package may import only fleetgrpc + fleetpaths + version (and
// grpc). It must NEVER import the server-only internals (internal/state,
// internal/backend, internal/create, internal/control, internal/flog, or
// internal/server). That rule is what keeps a remote client possible and is
// enforced by the depguard rule in .golangci.yml.
package fleetclient

import (
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Endpoint abstracts WHERE the server is and HOW we reach it, so commands never
// see a socket path or address. Locality is a property of the endpoint:
// auto-spawn is only valid for a local endpoint (you can't fork-exec a process
// on someone else's machine).
type Endpoint interface {
	// Target is the gRPC dial target (scheme-qualified).
	Target() string
	// IsLocal reports whether this endpoint is the local unix socket (and thus
	// whether auto-spawn / version-restart are permitted).
	IsLocal() bool
	String() string
	// DialOptions are the grpc.DialOptions for this endpoint (transport creds, and
	// for a gateway endpoint the CONNECT dialer + per-RPC token).
	DialOptions() []grpc.DialOption
}

// insecureCreds is the transport for unix-socket / plain-TCP endpoints (and the
// inner h2c of a gateway tunnel, whose outer TLS the dialer establishes).
func insecureCreds() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
}

// localEndpoint is the per-user unix socket; auto-spawnable.
type localEndpoint struct{ socket string }

func (e localEndpoint) Target() string                 { return "unix://" + e.socket }
func (e localEndpoint) IsLocal() bool                  { return true }
func (e localEndpoint) String() string                 { return "unix:" + e.socket }
func (e localEndpoint) DialOptions() []grpc.DialOption { return insecureCreds() }

// remoteEndpoint is a plain TCP target (e.g. a remote TUI). Not auto-spawnable.
type remoteEndpoint struct{ addr string }

func (e remoteEndpoint) Target() string                 { return "dns:///" + e.addr }
func (e remoteEndpoint) IsLocal() bool                  { return false }
func (e remoteEndpoint) String() string                 { return e.addr }
func (e remoteEndpoint) DialOptions() []grpc.DialOption { return insecureCreds() }

// selectEndpoint picks the transport, in precedence order:
//   - FLEET_GATEWAY=https://gw:50051/<id> (or http://… behind a TLS-terminating
//     proxy) → through a fleet gateway (the bearer token from FLEET_TOKEN, or
//     ~/.fleet/mcp.token for a same-host user).
//   - FLEET_SERVER=host:port → a plain remote TCP target.
//   - otherwise → the local auto-spawned unix socket.
//
// It errors only when FLEET_GATEWAY is set but malformed. Remote endpoints
// (gateway/server) are not auto-spawned and a version mismatch is a hard error.
func selectEndpoint() (Endpoint, error) {
	if gw := os.Getenv("FLEET_GATEWAY"); gw != "" {
		return newGatewayEndpoint(gw, gatewayToken())
	}
	if addr := os.Getenv("FLEET_SERVER"); addr != "" {
		return remoteEndpoint{addr: addr}, nil
	}
	return localEndpoint{socket: fleetpaths.SocketPath()}, nil
}

// gatewayToken resolves the bearer token for a gateway endpoint: FLEET_TOKEN, or
// the on-disk MCP token (so a user on the same host as their daemon need only
// supply the URL).
func gatewayToken() string {
	if t := os.Getenv("FLEET_TOKEN"); t != "" {
		return strings.TrimSpace(t)
	}
	if data, err := os.ReadFile(fleetpaths.McpTokenPath()); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}
