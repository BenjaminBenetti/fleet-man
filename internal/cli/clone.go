package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

// newCloneCmd returns the `fleet clone` command which duplicates an
// existing instance, preserving manually-installed software inside the
// source container. Only backends that report SupportsClone == true
// can be cloned; the server fails the job otherwise.
func newCloneCmd() *cobra.Command {
	var repoFlag string

	cmd := &cobra.Command{
		Use:   "clone <source> <destination>",
		Short: "Clone an existing instance into a new one",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcName := args[0]
			destName := args[1]

			srcTarget, err := fleet.Resolve(srcName, repoFlag)
			if err != nil {
				return err
			}

			fmt.Printf("Cloning %s/%s -> %s/%s...\n", srcTarget.Fleet, srcTarget.Instance, srcTarget.Fleet, destName)

			// The server pre-creates the StatusCloning destination (copying the
			// source's config/backend/tag/color/branch) and runs the clone — it
			// errors NotFound for a missing source / AlreadyExists for a taken dest.
			if err := runInstanceJob(cmd.Context(), func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				return svc.CloneInstance(ctx, &fleetgrpc.CloneInstanceRequest{
					Fleet:          srcTarget.Fleet,
					SourceInstance: srcTarget.Instance,
					NewInstance:    destName,
				})
			}); err != nil {
				return err
			}

			if st, serr := fetchFleetState(cmd.Context()); serr == nil {
				if f := st.GetFleets()[srcTarget.Fleet]; f != nil {
					for _, inst := range f.GetInstances() {
						if inst.GetName() == destName && inst.GetContainerId() != "" {
							cid := inst.GetContainerId()
							fmt.Printf("Instance %s/%s is running (container: %s)\n", srcTarget.Fleet, destName, cid[:min(12, len(cid))])
						}
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFlag, "repo", "", "Git remote URL identifying the fleet (defaults to the source's fleet)")
	return cmd
}
