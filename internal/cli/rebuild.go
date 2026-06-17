package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

func newRebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rebuild <name>",
		Aliases: []string{"rb"},
		Short:   "Rebuild an instance's container",
		Long: "Tear down and reprovision an instance's container without recreating the instance — " +
			"handy after editing devcontainer.json. The workspace (git checkout and uncommitted edits) " +
			"is preserved. Only backends with a rebuild primitive are supported (devcontainer, codespaces).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			fmt.Printf("Rebuilding %s/%s...\n", target.Fleet, target.Instance)
			// The server validates the target, refuses an unsupported backend, and
			// reprovisions the container in place (it errors NotFound / FailedPrecondition
			// for a missing or non-rebuildable target).
			if err := runInstanceJob(cmd.Context(), func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				return svc.RebuildInstance(ctx, &fleetgrpc.RebuildInstanceRequest{Fleet: target.Fleet, Instance: target.Instance})
			}); err != nil {
				return err
			}
			fmt.Printf("Instance %s/%s rebuilt.\n", target.Fleet, target.Instance)
			return nil
		},
	}
}
