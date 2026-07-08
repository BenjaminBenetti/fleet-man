package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"google.golang.org/grpc"
)

// jobs.go routes the create/destroy CLI commands (up/down/destroy/clone) through
// the fleet server's lifecycle jobs, so the server is the single writer for them
// too — the cross-process half of the issue #63 fix (concurrent `fleet up` no
// longer race state.json; they serialize through the one server).
//
// runInstanceJob is a package var so unit tests can stub the whole job layer.
var runInstanceJob = func(ctx context.Context, open func(context.Context, fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error)) error {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := open(ctx, conn.Service())
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return err
		}
		if d := ev.GetDone(); d != nil {
			for _, w := range d.GetWarnings() {
				fmt.Printf("warning: %s\n", w)
			}
			if !d.GetSuccess() {
				return fmt.Errorf("%s", d.GetError())
			}
			return nil
		}
	}
}
