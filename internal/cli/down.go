package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/spf13/cobra"
)

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down <name>",
		Short: "Stop and remove an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, st, f, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			start := time.Now()
			// Stop the container
			fmt.Printf("Stopping %s/%s...\n", target.Fleet, target.Instance)
			instanceBackend := backendutil.New(instance.Backend, false)
			if err := instanceBackend.Down(instance.ContainerID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to remove container: %v\n", err)
			}

			// Remove the workspace directory
			if instance.WorkspaceDir != "" {
				if err := os.RemoveAll(instance.WorkspaceDir); err != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to remove workspace dir: %v\n", err)
				}
			}

			// Remove from state
			if err := f.RemoveInstance(target.Instance); err != nil {
				return err
			}

			if err := state.Save(st); err != nil {
				return err
			}

			flog.Info("instance deleted", "fleet", target.Fleet, "instance", target.Instance, "ms", flog.MillisSince(start))
			fmt.Printf("Instance %s/%s removed.\n", target.Fleet, target.Instance)
			return nil
		},
	}
}
