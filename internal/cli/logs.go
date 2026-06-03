package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show logs for an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}
			// The server runs the backend logs command and streams it back.
			return streamLogs(cmd.Context(), target.Fleet, target.Instance, follow)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	return cmd
}
