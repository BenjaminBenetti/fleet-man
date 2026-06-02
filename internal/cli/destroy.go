package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

func newDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <fleet>",
		Short: "Remove a fleet and all its instances",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName := args[0]

			// Snapshot the instance count for the summary message (server-read).
			count := 0
			if st, err := fetchFleetState(cmd.Context()); err == nil {
				if f := st.GetFleets()[fleetName]; f != nil {
					count = len(f.GetInstances())
				}
			}

			// One destroy_fleet job: down every container, remove every workspace,
			// then remove the fleet record. Errors NotFound for a missing fleet.
			if err := runInstanceJob(cmd.Context(), func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				return svc.DestroyInstance(ctx, &fleetgrpc.DestroyInstanceRequest{Fleet: fleetName, DestroyFleet: true})
			}); err != nil {
				return err
			}

			fmt.Printf("Fleet %s destroyed (%d instances removed).\n", fleetName, count)
			return nil
		},
	}
}
