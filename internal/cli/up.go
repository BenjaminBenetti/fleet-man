package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

func newUpCmd() *cobra.Command {
	var repoFlag string
	var backendFlag string
	var branchFlag string

	cmd := &cobra.Command{
		Use:   "up <name>",
		Short: "Spawn a new instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			backendType, err := fleet.ParseBackendType(backendFlag)
			if err != nil {
				return err
			}

			target, err := fleet.Resolve(name, repoFlag)
			if err != nil {
				return err
			}

			// Resolve the remote URL: explicit --repo wins; otherwise reuse the
			// fleet's recorded remote (read via the server) and finally fall back
			// to the cwd's git remote for a brand-new fleet. The server pre-creates
			// the StatusCreating record and provisions — no client-side state write
			// (the #63 fix; concurrent `fleet up` now serialize through the server).
			remoteURL := repoFlag
			if remoteURL == "" {
				if st, serr := fetchFleetState(cmd.Context()); serr == nil {
					if f := st.GetFleets()[target.Fleet]; f != nil {
						remoteURL = f.GetRemote()
					}
				}
				if remoteURL == "" {
					remoteURL, err = fleet.RemoteURLFromCwd()
					if err != nil {
						return fmt.Errorf("could not determine repo URL: %w", err)
					}
				}
			}

			fmt.Printf("Creating %s/%s (backend: %s)...\n", target.Fleet, target.Instance, backendType)

			req := &fleetgrpc.CreateInstanceRequest{
				Fleet:    target.Fleet,
				Instance: target.Instance,
				Backend:  protoconv.BackendToProto(backendType),
				Verbose:  true,
			}
			if remoteURL != "" {
				req.Remote = &remoteURL
			}
			if branchFlag != "" {
				req.Branch = &branchFlag
			}
			if err := runInstanceJob(cmd.Context(), func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				return svc.CreateInstance(ctx, req)
			}); err != nil {
				return err
			}

			if st, serr := fetchFleetState(cmd.Context()); serr == nil {
				if f := st.GetFleets()[target.Fleet]; f != nil {
					for _, inst := range f.GetInstances() {
						if inst.GetName() == target.Instance {
							cid := inst.GetContainerId()
							fmt.Printf("Instance %s/%s is running (container: %s)\n", target.Fleet, target.Instance, cid[:min(12, len(cid))])
						}
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFlag, "repo", "", "Git remote URL to clone, or file:///abs/dir to copy a local template (needs <fleet>/<name>)")
	cmd.Flags().StringVar(&backendFlag, "backend", "devcontainer", "Backend type: devcontainer, coder, or codespaces")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Git branch to check out (defaults to the repository's default branch)")
	return cmd
}
