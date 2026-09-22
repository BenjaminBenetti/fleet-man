package fleetclient

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
)

// agent_forward.go holds the client-side pieces of SSH-agent forwarding that
// the CLI and the TUI share: the env var a TUI hands its children, and a
// registry probe that — unlike DialLocal — can never start or restart the
// local daemon.

// EnvAgentProviderPID is how a TUI tells the `fleet shell` children it spawns
// (directly, and through the tmux pane environment) that it already provides
// the user's ssh-agent to the remote they connect to:
// FLEET_AGENT_PROVIDER_PID=<the TUI's pid>. While that process lives a child
// starts no provider of its own. Both read the same registry and serve the
// same agent, so a second provider would only become the newest one and push
// the TUI's to standby for as long as the child runs.
const EnvAgentProviderPID = "FLEET_AGENT_PROVIDER_PID"

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
