package cli

import (
	"github.com/spf13/cobra"

	"github.com/BenjaminBenetti/fleet-man/internal/browserproxy"
)

// newBrowserProxyCmd creates the hidden `fleet browser-proxy` command: the
// HTTP/CONNECT proxy the host's browser reaches an instance's localhost
// through (TUI 'b'). fleet starts it inside the instance with the staged
// binary, as it does the landing page; on the host it is meaningless.
func newBrowserProxyCmd() *cobra.Command {
	var addr string
	var port int
	cmd := &cobra.Command{
		Use:    "browser-proxy",
		Short:  "Serve the instance's browser proxy (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return browserproxy.Run(cmd.Context(), browserproxy.Config{Addr: addr, Port: port, Log: cmd.ErrOrStderr()})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "address to listen on (default every interface)")
	cmd.Flags().IntVar(&port, "port", browserproxy.DefaultPort, "port to listen on")
	return cmd
}
