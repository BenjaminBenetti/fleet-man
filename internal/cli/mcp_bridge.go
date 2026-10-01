package cli

import (
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
	"github.com/spf13/cobra"
)

// newMCPBridgeCmd creates the hidden `fleet mcp-bridge` command: the stdio
// MCP server the coding agents in an instance are given when its fleet has the
// Fleet MCP setting on (issue #219). It runs INSIDE an instance, through the
// staged /usr/bin/fleet, and relays to the daemon's MCP socket in the
// instance's control directory — meaningless on the host, hence hidden.
func newMCPBridgeCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:    mcpbridge.Subcommand,
		Short:  "Relay the fleet MCP server to stdio inside an instance (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		// Its stderr is the MCP client's server log: no usage dump there.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			b := &mcpbridge.Bridge{
				Dial: mcpbridge.Dialer(socket),
				In:   os.Stdin,
				Out:  os.Stdout,
			}
			return b.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&socket, "socket", mcpbridge.ContainerSocketPath, "the daemon's MCP socket")
	return cmd
}
