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

			// No --backend leaves the target to the daemon: its remembered
			// default (the target last picked in a TUI New instance dialog),
			// else devcontainer.
			backend := fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED
			if backendFlag != "" {
				backendType, err := fleet.ParseBackendType(backendFlag)
				if err != nil {
					return err
				}
				backend = protoconv.BackendToProto(backendType)
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

			req := &fleetgrpc.CreateInstanceRequest{
				Fleet:    target.Fleet,
				Instance: target.Instance,
				Backend:  backend,
				Verbose:  true,
			}
			if remoteURL != "" {
				req.Remote = &remoteURL
			}
			if branchFlag != "" {
				req.Branch = &branchFlag
			}
			out := cmd.OutOrStdout()
			if err := runInstanceJob(cmd.Context(), true, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				stream, err := svc.CreateInstance(ctx, req)
				if err != nil {
					return nil, err
				}
				// Announced once the daemon has accepted the create, so the line
				// names the target it resolved, not one guessed here.
				return onJobStarted(stream, func(started *fleetgrpc.JobStarted) {
					fmt.Fprintf(out, "Creating %s/%s (backend: %s)...\n", target.Fleet, target.Instance, startedTargetLabel(started, backendFlag))
				}), nil
			}); err != nil {
				return err
			}

			if st, serr := fetchFleetState(cmd.Context()); serr == nil {
				if f := st.GetFleets()[target.Fleet]; f != nil {
					for _, inst := range f.GetInstances() {
						if inst.GetName() == target.Instance {
							fmt.Printf("Instance %s/%s is running (container: %s)\n", target.Fleet, target.Instance, fleet.ShortContainerID(inst.GetContainerId()))
						}
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFlag, "repo", "", "Git remote URL to clone, or file:///abs/dir to copy a local template (needs <fleet>/<name>)")
	cmd.Flags().StringVar(&backendFlag, "backend", "", "Backend: devcontainer, coder, or codespaces (default: fleetd's remembered target)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Git branch to check out (defaults to the repository's default branch)")
	return cmd
}
