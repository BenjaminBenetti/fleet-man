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

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
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
}

// localEndpoint is the per-user unix socket; auto-spawnable.
type localEndpoint struct{ socket string }

func (e localEndpoint) Target() string { return "unix://" + e.socket }
func (e localEndpoint) IsLocal() bool  { return true }
func (e localEndpoint) String() string { return "unix:" + e.socket }

// remoteEndpoint is a future TCP target (e.g. a remote TUI). Not auto-spawnable.
type remoteEndpoint struct{ addr string }

func (e remoteEndpoint) Target() string { return "dns:///" + e.addr }
func (e remoteEndpoint) IsLocal() bool  { return false }
func (e remoteEndpoint) String() string { return e.addr }

// selectEndpoint picks the transport. FLEET_SERVER=host:port forces a remote
// endpoint (no auto-spawn, version mismatch is a hard error); otherwise the
// local auto-spawned unix socket.
func selectEndpoint() Endpoint {
	if addr := os.Getenv("FLEET_SERVER"); addr != "" {
		return remoteEndpoint{addr: addr}
	}
	return localEndpoint{socket: fleetpaths.SocketPath()}
}
