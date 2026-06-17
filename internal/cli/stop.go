package cli

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop an instance without removing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			result, err := instanceops.StopInstance(target.Fleet, target.Instance)
			if err != nil {
				return err
			}

			if !result.Changed {
				fmt.Printf("Instance %s/%s is already stopped.\n", target.Fleet, target.Instance)
				return nil
			}

			fmt.Printf("Instance %s/%s stopped.\n", target.Fleet, target.Instance)
			return nil
		},
	}
}
