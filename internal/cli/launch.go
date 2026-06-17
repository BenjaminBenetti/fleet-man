package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/launchtui"
	"github.com/spf13/cobra"
)

// newLaunchCmd creates the user-facing `fleet launch` command.
//
// Unlike most fleet commands it is meant to be run INSIDE an instance: it reads
// the workspace's customizations.fleet.fleetLaunch block (or an explicit
// --config devcontainer.json). Activating a link or app drives the HOST browser
// — fleet bind-mounts a control socket into the instance, and the in-instance
// launcher sends "open this URL" messages to the host fleet TUI over it (see
// internal/control and internal/launchtui).
//
// Three forms:
//   - `fleet launch`        opens the interactive grid TUI.
//   - `fleet launch list`   prints the configured links and apps, then exits.
//   - `fleet launch <name>` opens that link or app directly, as if clicked.
func newLaunchCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "launch [name|list]",
		Short: "Open an instance's links and apps",
		Long: "Open a terminal UI, inside an instance, that lists the links and apps " +
			"configured in customizations.fleet.fleetLaunch and drives the host browser " +
			"to them over the control socket fleet mounts into the instance.\n\n" +
			"With no argument it opens the interactive grid. `fleet launch list` prints the " +
			"configured links and apps and exits. `fleet launch <name>` opens the named link " +
			"or app directly, as if it were clicked in the grid.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := launchtui.Config{ConfigPath: configPath}
			switch {
			case len(args) == 0:
				return launchtui.Run(cfg)
			case args[0] == "list":
				return launchtui.List(cfg, cmd.OutOrStdout())
			default:
				return launchtui.LaunchByName(cfg, args[0], cmd.ErrOrStderr())
			}
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "devcontainer.json to read (default: auto-detect by searching the current directory, then each parent, for one with a customizations.fleet block)")

	return cmd
}
