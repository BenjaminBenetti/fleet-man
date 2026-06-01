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

func newDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <fleet>",
		Short: "Remove a fleet and all its instances",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName := args[0]

			st, err := state.Load()
			if err != nil {
				return err
			}

			f, ok := st.Fleets[fleetName]
			if !ok {
				return fmt.Errorf("fleet %q not found", fleetName)
			}

			start := time.Now()
			for _, instance := range f.Instances {
				fmt.Printf("Stopping %s/%s...\n", fleetName, instance.Name)
				instanceBackend := backendutil.New(instance.Backend, false)
				if err := instanceBackend.Down(instance.ContainerID); err != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to remove container for %s: %v\n", instance.Name, err)
				}
				if instance.WorkspaceDir != "" {
					if err := os.RemoveAll(instance.WorkspaceDir); err != nil {
						fmt.Fprintf(os.Stderr, "warning: failed to remove workspace for %s: %v\n", instance.Name, err)
					}
				}
			}

			delete(st.Fleets, fleetName)

			if err := state.Save(st); err != nil {
				return err
			}

			flog.Info("fleet destroyed", "fleet", fleetName, "instances", len(f.Instances), "ms", flog.MillisSince(start))
			fmt.Printf("Fleet %s destroyed (%d instances removed).\n", fleetName, len(f.Instances))
			return nil
		},
	}
}
