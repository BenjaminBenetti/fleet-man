package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/landingpage"
	"github.com/spf13/cobra"
)

// newLandingPageCmd creates the hidden `fleet landing-page` command.
//
// This is not a user-facing command: fleet-man injects its own binary
// into an instance and runs `fleet landing-page` there to serve the
// browser landing page (see internal/landingpage and the browser launch
// flow). It is hidden from --help because running it on the host is
// meaningless — it reads the local devcontainer.json's
// customizations.fleet.browser.landingPage block and serves it.
func newLandingPageCmd() *cobra.Command {
	var port int
	var workspace string

	cmd := &cobra.Command{
		Use:    "landing-page",
		Short:  "Serve the browser landing page (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return landingpage.Run(landingpage.Config{
				Port:         port,
				WorkspaceDir: workspace,
			})
		},
	}

	cmd.Flags().IntVar(&port, "port", landingpage.DefaultPort, "port to listen on")
	cmd.Flags().StringVar(&workspace, "workspace", ".", "workspace dir whose devcontainer.json configures the page")

	return cmd
}
