package cli

import (
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/spf13/cobra"
)

// exitPodInit ends `fleet pod-init` with the start script's status; a package
// var so tests can catch it.
var exitPodInit = os.Exit

// newPodInitCmd creates the hidden in-pod `fleet pod-init`: PID 1 of a
// Kubernetes instance pod (see internal/podinit).
func newPodInitCmd() *cobra.Command {
	spec := podinit.DefaultSpecPath
	cmd := &cobra.Command{
		Use:           "pod-init",
		Short:         "Run as PID 1 of an instance pod (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			exitPodInit(podinit.Run(spec, os.Stdout, os.Stderr))
			return nil
		},
	}
	cmd.Flags().StringVar(&spec, "spec", spec, "pid1 spec file")
	return cmd
}
