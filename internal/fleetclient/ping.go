package fleetclient

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc"
)

// pingTimeout bounds one Ping round trip. Long enough for a cold TLS handshake
// through a far-away gateway, short enough that a status sweep over several
// registered remotes stays snappy.
const pingTimeout = 3 * time.Second

// Ping dials a fleet-armada remote (a FLEET_GATEWAY-style URL plus its bearer
// token, or an ssh:// URL whose token is discovered by the local daemon) and
// runs one Hello round trip, validating connectivity, routing, and daemon auth
// in a single RPC. It deliberately skips the version reconcile that Dial runs,
// so a version-skewed remote still answers and the caller can render its real
// reachability (and compare ServerVersion itself).
//
// The gRPC status code distinguishes the failure for status indicators:
// Unavailable = gateway/tunnel unreachable, NotFound = unknown session or the
// remote daemon is offline / has Remote Fleet disabled, Unauthenticated = bad
// token, FailedPrecondition = the local daemon could not bring the ssh tunnel up
// (its message says why).
func Ping(ctx context.Context, remoteURL, token string) (*fleetgrpc.HelloReply, error) {
	var ep Endpoint
	var err error
	if IsSSHURL(remoteURL) {
		ep, err = newSSHEndpoint(ctx, remoteURL)
	} else {
		ep, err = newGatewayEndpoint(remoteURL, token)
	}
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(ep.Target(), ep.DialOptions()...)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	hctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return fleetgrpc.NewFleetServiceClient(conn).Hello(hctx, &fleetgrpc.HelloRequest{ClientVersion: version.Version})
}
