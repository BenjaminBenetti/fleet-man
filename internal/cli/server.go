package cli

import (
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/server"
	"github.com/spf13/cobra"
)

// newServerCmd runs the fleet server daemon. It is normally auto-started by the
// first client that can't reach a running server (see internal/fleetclient), so
// it is hidden from the help output; users don't invoke it directly.
//
// This is the ONE client-side file permitted to import internal/server — it is
// the entrypoint that runs the daemon, not a client of it.
func newServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "server",
		Short:  "Run the fleet server daemon (normally auto-started)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if note := os.Getenv(fleetclient.EnvSpawnNote); note != "" {
				// Ours alone: the daemon's children must not inherit it.
				_ = os.Unsetenv(fleetclient.EnvSpawnNote)
				flog.Warn("fleet server replaces an unresponsive one", "previous", note)
			}
			return server.Serve(cmd.Context())
		},
	}
}
