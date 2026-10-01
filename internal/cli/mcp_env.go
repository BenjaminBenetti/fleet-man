package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/agentstrategy"
	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
	"github.com/spf13/cobra"
)

// newMCPEnvCmd creates the hidden `fleet mcp-env` command: what an instance's
// shells run (from fleet.rc, when the fleet has the Fleet MCP setting on) to
// point the coding agents started from them at the fleet MCP (issue #219). It
// writes each supported agent's setup — for Claude Code, a plugin holding the
// MCP server (`fleet mcp-bridge`) and the Fleet Admiral skill — under the
// user's cache directory and prints the exports to eval. Run INSIDE an
// instance through the staged /usr/bin/fleet; meaningless on the host, hence
// hidden.
func newMCPEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp-env",
		Short:  "Print the shell exports that hand agents the fleet MCP (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		// Its output is eval'd by every shell: on failure print nothing there.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			cache, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			exports, err := agentstrategy.InstallFleetMCP(filepath.Join(cache, "fleet", "mcp"), []string{self, mcpbridge.Subcommand})
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), exports)
			return err
		},
	}
}
