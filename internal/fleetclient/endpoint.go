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
	"context"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The endpoint-selection environment variables. This package owns their
// semantics (selectEndpoint / IsRemote / IsGateway), so the names are defined
// here once; other packages that read or set them (the TUI's armada switch,
// admiralmcp's local-only guard) refer to these constants.
const (
	// EnvGateway points the client through a fleet gateway:
	// FLEET_GATEWAY=https://gw:50051/<id>.
	EnvGateway = "FLEET_GATEWAY"
	// EnvServer points the client at a plain remote TCP daemon:
	// FLEET_SERVER=host:port.
	EnvServer = "FLEET_SERVER"
	// EnvSSH points the client at a daemon reached over an SSH tunnel the LOCAL
	// daemon maintains: FLEET_SSH=ssh://[user@]host[:port]. Resolved to a
	// loopback address + token per dial via the local ResolveArmadaRemote RPC.
	EnvSSH = "FLEET_SSH"
	// EnvToken carries the bearer token for a gateway/remote daemon; falls back
	// to ~/.fleet/mcp.token (see BearerToken).
	EnvToken = "FLEET_TOKEN"
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
//   - FLEET_SSH=ssh://[user@]host[:port] → through an SSH tunnel the local
//     daemon maintains (resolved per dial; needs the local daemon, which
//     auto-spawns).
//   - FLEET_SERVER=host:port → a plain remote TCP target.
//   - otherwise → the local auto-spawned unix socket.
//
// It errors when FLEET_GATEWAY/FLEET_SSH is set but malformed, or the SSH
// tunnel cannot be brought up. Remote endpoints are not auto-spawned and a
// version mismatch is a hard error.
func selectEndpoint(ctx context.Context) (Endpoint, error) {
	if gw := os.Getenv(EnvGateway); gw != "" {
		return newGatewayEndpoint(gw, gatewayToken())
	}
	if ssh := os.Getenv(EnvSSH); ssh != "" {
		return newSSHEndpoint(ctx, ssh)
	}
	if addr := os.Getenv(EnvServer); addr != "" {
		return remoteEndpoint{addr: addr}, nil
	}
	return localEndpoint{socket: fleetpaths.SocketPath()}, nil
}

// IsRemote reports whether the client is pointed at a remote daemon
// (FLEET_GATEWAY, FLEET_SSH or FLEET_SERVER set) rather than the local
// auto-spawned socket. Client-host concerns — dependency checks, the local-exec
// TTY carve-out — only apply when this is false.
func IsRemote() bool {
	return os.Getenv(EnvGateway) != "" || os.Getenv(EnvSSH) != "" || os.Getenv(EnvServer) != ""
}

// IsSSH reports whether the client reaches the daemon over an SSH tunnel
// (FLEET_SSH set).
func IsSSH() bool {
	return os.Getenv(EnvSSH) != ""
}

// IsSSHURL reports whether a fleet-armada URL names an SSH remote (ssh://…)
// rather than a gateway. The scheme decides the transport everywhere a remote
// URL is handled (registry, switch, ping, labels).
func IsSSHURL(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "ssh://")
}

// IsGateway reports whether the client reaches the daemon THROUGH a fleet
// gateway (FLEET_GATEWAY set), as opposed to a direct remote TCP target
// (FLEET_SERVER) or the local socket. Only a gateway connection has a gateway
// version to show in the control chain.
func IsGateway() bool {
	return os.Getenv(EnvGateway) != ""
}

// gatewayToken resolves the bearer token for a gateway endpoint: FLEET_TOKEN, or
// the on-disk MCP token (so a user on the same host as their daemon need only
// supply the URL).
func gatewayToken() string {
	if t := os.Getenv(EnvToken); t != "" {
		return strings.TrimSpace(t)
	}
	if data, err := os.ReadFile(fleetpaths.McpTokenPath()); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

// BearerToken resolves the bearer token used to authenticate to a fleet daemon:
// FLEET_TOKEN if set, else the on-disk MCP token (~/.fleet/mcp.token). It is the
// same secret the MCP HTTP server and the remote-fleet gRPC surface accept, so
// callers (e.g. the Settings page) can hand it out for copy-paste setup of
// remote clients. Returns "" when neither source yields a token.
func BearerToken() string {
	return gatewayToken()
}
