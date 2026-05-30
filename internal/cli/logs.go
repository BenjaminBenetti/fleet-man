package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show logs for an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, _, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			instanceBackend := backendutil.New(instance.Backend, false)
			return instanceBackend.Logs(instance.ContainerID, follow)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	return cmd
}
