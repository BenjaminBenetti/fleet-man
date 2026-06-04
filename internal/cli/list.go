package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
	"github.com/spf13/cobra"
)

var (
	listOutput     io.Writer = os.Stdout
	listBranchName           = gitutil.BranchName
)

// fetchFleetState retrieves the current state from the fleet server. It is a
// package var so unit tests can inject a canned snapshot instead of spinning up
// a real server. list and status both go through it — they are the first
// commands migrated onto the client/server path (Phase 1).
var fetchFleetState = func(ctx context.Context) (*fleetgrpc.State, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	reply, err := conn.Service().GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return nil, err
	}
	return reply.GetState(), nil
}

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list [fleet]",
		Aliases: []string{"ls"},
		Short:   "List devcontainer instances",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := fetchFleetState(cmd.Context())
			if err != nil {
				return err
			}

			var fleetFilter string
			if len(args) == 1 {
				fleetFilter = args[0]
			} else {
				// Try to infer from cwd
				fleetFilter, _ = fleet.FleetNameFromCwd()
			}

			w := tabwriter.NewWriter(listOutput, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "FLEET\tINSTANCE\tSTATUS\tCONTAINER\tCREATED\tBRANCH")

			for name, f := range st.GetFleets() {
				if fleetFilter != "" && name != fleetFilter {
					continue
				}
				for _, instance := range f.GetInstances() {
					containerShort := instance.GetContainerId()
					if len(containerShort) > 12 {
						containerShort = containerShort[:12]
					}
					created := ""
					if ts := instance.GetCreatedAt(); ts != nil {
						created = ts.AsTime().Local().Format("2006-01-02 15:04")
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						name,
						instance.GetName(),
						instance.GetStatus().Display(),
						containerShort,
						created,
						listBranchName(instance.GetWorkspaceDir()),
					)
				}
			}

			w.Flush()
			return nil
		},
	}

	return cmd
}
