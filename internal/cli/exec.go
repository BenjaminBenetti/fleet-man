package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <name> <command...>",
		Short: "Execute a command inside an instance",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}
			// The server resolves the backend exec argv; we run it locally with
			// inherited stdio (the TTY carve-out — no direct backend access).
			return runResolvedExec(cmd.Context(), target.Fleet, target.Instance, args[1:])
		},
	}
}
