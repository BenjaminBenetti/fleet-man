package cli

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show fleet-wide status summary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := fetchFleetState(cmd.Context())
			if err != nil {
				return err
			}

			fleets := st.GetFleets()
			if len(fleets) == 0 {
				fmt.Println("No fleets. Use 'fleet up <name>' to create an instance.")
				return nil
			}

			var total fleetgrpc.StatusCounts
			for name, f := range fleets {
				counts := fleetgrpc.CountInstanceStatuses(f.GetInstances())
				total.Merge(counts)
				fmt.Printf("%s: %d instances (%s) — %s\n", name, counts.Total(), formatStatusCounts(counts), f.GetRemote())
			}

			fmt.Printf("\nTotal: %d fleets, %d instances (%s)\n", len(fleets), total.Total(), formatStatusCounts(total))
			return nil
		},
	}
}

func formatStatusCounts(c fleetgrpc.StatusCounts) string {
	if c.Other > 0 {
		return fmt.Sprintf("%d running, %d stopped, %d other", c.Running, c.Stopped, c.Other)
	}
	return fmt.Sprintf("%d running, %d stopped", c.Running, c.Stopped)
}
