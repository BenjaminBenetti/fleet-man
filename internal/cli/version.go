package cli

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"github.com/spf13/cobra"
)

// newVersionCmd creates the `fleet version` command. It prints the
// compiled-in version string on its own line — empty for dev builds where
// version.Version was never set via the release-time ldflag.
//
// The output shape is load-bearing: fleet-man's TUI launches the landing
// page by injecting its own binary into an instance, and uses this
// command to compare the in-container binary's version against the host's
// so it can refresh a stale copy. The contract there is:
//
//   - non-zero exit  → pre-version binary that doesn't know this command.
//   - empty stdout   → dev build (no version baked in).
//   - non-empty line → that version.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the fleet version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return nil
		},
	}
}
