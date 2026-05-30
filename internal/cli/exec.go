package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <name> <command...>",
		Short: "Execute a command inside an instance",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, _, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			instanceBackend := backendutil.NewForInstance(instance, false)
			return instanceBackend.Exec(instance.WorkspaceDir, args[1:])
		},
	}
}
