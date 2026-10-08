package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/agentstrategy"
	"github.com/spf13/cobra"
)

// newClaudeModEnvCmd creates the hidden `fleet claude-mod-env` command: what an
// instance's shells run (from fleet.rc) to load the fleet status mod into the
// Claude Code sessions started from them. It writes the mod, which ships inside
// this binary, under the user's cache directory and prints the exports to eval.
// The mod draws only while the daemon keeps its status file in the control
// directory (the global "Fleet status mod" setting), so installing it whatever
// the setting is lets the setting take effect in running sessions. Run INSIDE
// an instance through the staged /usr/bin/fleet; meaningless on the host, hence
// hidden.
func newClaudeModEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "claude-mod-env",
		Short:  "Print the shell exports that load the fleet status mod into Claude Code (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		// Its output is eval'd by every shell: on failure print nothing there.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cache, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			exports, err := agentstrategy.InstallStatusMod(filepath.Join(cache, "fleet", "claude-mod"))
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), exports)
			return err
		},
	}
}
