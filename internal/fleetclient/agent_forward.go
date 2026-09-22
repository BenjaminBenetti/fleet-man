package fleetclient

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
)

// agent_forward.go holds the client side of SSH-agent forwarding that lives
// outside the CLI: a registry probe that — unlike DialLocal — can never start
// or restart the local daemon.

// ProbeLocalArmada reads the fleet-armada registry from the LOCAL daemon if
// one is listening right now. Unlike DialLocal it never spawns, restarts or
// version-reconciles a daemon (no Hello, no spawn lock): a caller that only
// wants to know what the registry says (should this command forward the
// agent?) must not be the reason a gateway-only user gets a daemon spawned,
// or the reason a running one is drained and relaunched under its other
// clients. It fails fast when nothing listens; otherwise ctx bounds it.
func ProbeLocalArmada(ctx context.Context) ([]*fleetgrpc.ArmadaRemote, error) {
	ep := localEndpoint{socket: fleetpaths.SocketPath()}
	conn, err := grpc.NewClient(ep.Target(), ep.DialOptions()...)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// No WaitForReady: a socket nobody listens on must fail the RPC at once
	// rather than hold it until ctx expires.
	reply, err := fleetgrpc.NewFleetServiceClient(conn).GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		return nil, err
	}
	return reply.GetRemotes(), nil
}
