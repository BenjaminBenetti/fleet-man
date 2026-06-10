package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
			"presents the public cert. Set --public-url's scheme to match (https vs http).\n\n" +
			"Set --session-key (or FLEET_GATEWAY_SESSION_KEY) to a stable secret so fleet\n" +
			"daemons keep their session URL across gateway restarts; without it each boot\n" +
			"uses a random key and a restart hands every daemon a fresh URL.\n\n" +
			"Every flag can also be set via environment variable — FLEET_GATEWAY_<FLAG>\n" +
			"with dashes as underscores (e.g. FLEET_GATEWAY_PUBLIC_URL,\n" +
			"FLEET_GATEWAY_MAX_SESSIONS) — handy for the Docker image and Kubernetes.\n" +
			"A flag given on the command line wins over its environment variable.",
		Args: cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return applyGatewayEnv(cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return gateway.Serve(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.PublicAddr, "public-addr", ":443", "address MCP agents connect to (HTTPS when a cert is set, else HTTP)")
	f.StringVar(&cfg.GRPCAddr, "grpc-addr", ":50051", "address for native gRPC + fleetd registration (h2c when cert-less, h2 under TLS); empty disables both")
	f.StringVar(&cfg.PublicURL, "public-url", "", "external base URL agents use, e.g. https://gw.example.com or http://gw.example.com (required)")
	f.StringVar(&cfg.PublicGRPCURL, "public-grpc-url", "", "external base URL of the gRPC endpoint that remote `fleet` clients dial, e.g. https://gw.example.com:50051; daemons that enable remote fleet are handed <public-grpc-url>/grpc/<id> as their Public GRPC URL (empty = none)")
	f.StringVar(&cfg.TLSCert, "tls-cert", "", "path to the TLS certificate, PEM (optional; set with --tls-key to enable TLS)")
	f.StringVar(&cfg.TLSKey, "tls-key", "", "path to the TLS private key, PEM (optional; set with --tls-cert to enable TLS)")
	f.IntVar(&cfg.MaxSessions, "max-sessions", 0, "max concurrent tunnels (0 = default 1024)")
	f.StringVar(&cfg.SessionKey, "session-key", "", "secret key signing session-resume tokens, so daemons keep their session URL across gateway restarts (empty = random per boot; FLEET_GATEWAY_SESSION_KEY is also read)")

	return cmd
}

// applyGatewayEnv fills in any gateway flag not given on the command line from
// its FLEET_GATEWAY_<FLAG> environment variable (dashes become underscores, e.g.
// --public-url ← FLEET_GATEWAY_PUBLIC_URL), so the Docker image is configurable
// in Kubernetes without rebuilding the args list (issue #135). Explicit flags
// win; an env value that fails the flag's parser (e.g. a non-numeric
// FLEET_GATEWAY_MAX_SESSIONS) is an error naming the variable.
func applyGatewayEnv(fs *pflag.FlagSet) error {
	var err error
	fs.VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed || f.Name == "help" {
			return
		}
		env := "FLEET_GATEWAY_" + strings.ReplaceAll(strings.ToUpper(f.Name), "-", "_")
		val, ok := os.LookupEnv(env)
		if !ok {
			return
		}
		if setErr := fs.Set(f.Name, val); setErr != nil {
			err = fmt.Errorf("invalid %s: %w", env, setErr)
		}
	})
	return err
}
