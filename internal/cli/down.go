package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down <name>",
		Short: "Stop and remove an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			fmt.Printf("Stopping %s/%s...\n", target.Fleet, target.Instance)
			instance := target.Instance
			// The server tears down the container + workspace and removes the
			// record (it errors NotFound for a missing target).
			if err := runInstanceJob(cmd.Context(), func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				return svc.DestroyInstance(ctx, &fleetgrpc.DestroyInstanceRequest{Fleet: target.Fleet, Instance: &instance})
			}); err != nil {
				return err
			}
			fmt.Printf("Instance %s/%s removed.\n", target.Fleet, target.Instance)
			return nil
		},
	}
}
