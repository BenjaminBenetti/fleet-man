package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/create"
	"github.com/spf13/cobra"
)

// newCloneInstanceCmd returns the hidden `_clone-instance` subcommand
// the TUI spawns as a detached child to clone an instance in the
// background. The destination instance record is expected to already
// exist in state.json with StatusCloning so the TUI can render
// progress; this subcommand simply hands off to create.RunClone.
func newCloneInstanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_clone-instance",
		Short:  "Internal: clone an instance in the background",
		Hidden: true,
		Args:   cobra.ExactArgs(3), // fleetName srcInstance destInstance
		RunE: func(cmd *cobra.Command, args []string) error {
			return create.RunClone(args[0], args[1], args[2], false)
		},
	}
}
