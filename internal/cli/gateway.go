package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/spf13/cobra"
)

// newGatewayCmd runs the fleet gateway: the remote, operator-hosted relay that
// fleetd registers with to expose its MCP server to the internet (see
// internal/gateway). Unlike `fleet server` this is meant to be run directly by an
// operator on a public host, so it is NOT hidden.
//
// internal/cli normally must not import server-side packages (the depguard
// boundary), but internal/gateway is a self-contained server module with no
// fleetd internals, and this file is its thin entrypoint — mirroring how
// server.go is the lone entrypoint for internal/server.
func newGatewayCmd() *cobra.Command {
	var cfg gateway.Config

	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run the fleet gateway (remote MCP relay)",
		Long: "Run the fleet gateway: a public, operator-hosted server that relays remote\n" +
			"MCP traffic to fleet daemons over a reverse tunnel. fleet daemons dial in on\n" +
			"the control address; MCP agents connect over HTTPS at <public-url>/mcp/<id>.\n\n" +
			"Requires a TLS certificate (use a publicly-trusted cert, e.g. from Let's\n" +
			"Encrypt, so fleet daemons can verify it).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return gateway.Serve(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.ControlAddr, "control-addr", ":8443", "address fleet daemons dial in on (TLS)")
	f.StringVar(&cfg.PublicAddr, "public-addr", ":443", "address MCP agents connect to (HTTPS)")
	f.StringVar(&cfg.PublicURL, "public-url", "", "external base URL agents use, e.g. https://gw.example.com (required)")
	f.StringVar(&cfg.TLSCert, "tls-cert", "", "path to the TLS certificate, PEM (required)")
	f.StringVar(&cfg.TLSKey, "tls-key", "", "path to the TLS private key, PEM (required)")
	f.IntVar(&cfg.MaxSessions, "max-sessions", 0, "max concurrent tunnels (0 = default 1024)")

	return cmd
}
