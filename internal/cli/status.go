package cli

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show fleet status",
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

			totalInstances := 0
			running := 0
			stopped := 0
			other := 0

			for name, f := range fleets {
				fleetRunning := 0
				fleetStopped := 0
				fleetOther := 0
				for _, instance := range f.GetInstances() {
					totalInstances++
					switch instance.GetStatus() {
					case fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING:
						fleetRunning++
						running++
					case fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPED:
						fleetStopped++
						stopped++
					default:
						fleetOther++
						other++
					}
				}
				fmt.Printf("%s: %d instances (%s) — %s\n", name, len(f.GetInstances()), formatStatusCounts(fleetRunning, fleetStopped, fleetOther), f.GetRemote())
			}

			fmt.Printf("\nTotal: %d fleets, %d instances (%s)\n", len(fleets), totalInstances, formatStatusCounts(running, stopped, other))
			return nil
		},
	}
}

func formatStatusCounts(running, stopped, other int) string {
	if other > 0 {
		return fmt.Sprintf("%d running, %d stopped, %d other", running, stopped, other)
	}
	return fmt.Sprintf("%d running, %d stopped", running, stopped)
}
