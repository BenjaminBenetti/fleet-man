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
			"MCP and gRPC traffic to fleet daemons over a reverse tunnel. fleet daemons\n" +
			"register over the gRPC endpoint (--grpc-addr); MCP agents connect at\n" +
			"<public-url>/mcp/<id>, and remote `fleet` clients dial the gRPC endpoint.\n\n" +
			"TLS is optional. Provide both --tls-cert and --tls-key to terminate TLS in\n" +
			"the gateway (use a publicly-trusted cert, e.g. from Let's Encrypt, so fleet\n" +
			"daemons can verify it). Omit both to serve plain HTTP — intended for running\n" +
			"behind a TLS-terminating reverse proxy (e.g. Kubernetes/Traefik), which then\n" +
			"presents the public cert. Set --public-url's scheme to match (https vs http).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return gateway.Serve(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.PublicAddr, "public-addr", ":443", "address MCP agents connect to (HTTPS when a cert is set, else HTTP)")
	f.StringVar(&cfg.GRPCAddr, "grpc-addr", ":50051", "address for native gRPC + fleetd registration (h2c when cert-less, h2 under TLS); empty disables both")
	f.StringVar(&cfg.PublicURL, "public-url", "", "external base URL agents use, e.g. https://gw.example.com or http://gw.example.com (required)")
	f.StringVar(&cfg.TLSCert, "tls-cert", "", "path to the TLS certificate, PEM (optional; set with --tls-key to enable TLS)")
	f.StringVar(&cfg.TLSKey, "tls-key", "", "path to the TLS private key, PEM (optional; set with --tls-cert to enable TLS)")
	f.IntVar(&cfg.MaxSessions, "max-sessions", 0, "max concurrent tunnels (0 = default 1024)")

	return cmd
}
