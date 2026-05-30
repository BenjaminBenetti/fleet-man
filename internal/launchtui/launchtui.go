// Package launchtui is the in-instance Fleet Launch terminal UI run by
// `fleet launch`. It reads the workspace's customizations.fleet.fleetLaunch
// block and presents the configured Links (fleetLaunch.sites) and Apps
// (fleetLaunch.apps) as a flex grid of squares that wraps to the terminal
// width. Arrow keys and hjkl move a cursor; enter or a mouse click activates a
// square.
//
// The browser the TUI drives lives on the host (it is proxied into the
// container by privoxy), so the TUI cannot open it directly. Instead it dials
// the control socket fleet bind-mounts into the instance (internal/control)
// and asks the host fleet TUI to open or navigate the browser:
//
//   - activating a Link sends the link's URL straight to the host;
//   - activating an App first ensures the app's command is running on its port
//     locally (internal/appstart), waits for the port, then sends
//     http://localhost:<port> to the host.
//
// If the control socket cannot be dialled (the host fleet TUI isn't running)
// the program still renders so the user can browse the configured options; it
// shows a persistent status line and refuses to "open" anything until a host
// connection exists.
package launchtui

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Public entry point
// ===========================================

// Config parameterises Run. The zero value is the normal in-instance case:
// auto-detect the workspace's devcontainer.json and dial the standard control
// socket.
type Config struct {
	// ConfigPath is the devcontainer.json to read. Empty means auto-detect by
	// searching the current directory for .devcontainer/devcontainer.json or
	// ./devcontainer.json (LoadFleetCustomizations(".")). A non-empty path is
	// read directly (LoadFleetCustomizationsFromFile) and a missing file is an
	// error.
	ConfigPath string
	// SocketPath overrides the control socket the client dials. Empty means the
	// standard container-side path (control.ContainerSocketPath). Tests set
	// this to a temp socket.
	SocketPath string
}

// socketPath resolves the control socket to dial: the explicit SocketPath when
// set, otherwise the standard container-side path. Shared by Run and
// LaunchByName so both default the same way.
func (c Config) socketPath() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return control.ContainerSocketPath
}

// Run loads the fleetLaunch configuration, dials the host control socket, and
// runs the grid TUI until the user quits. It returns nil on a clean quit.
//
// Two early exits avoid an empty screen: a configuration that defines neither
// links nor apps prints a friendly note and returns (nothing to show), and a
// failed control dial is NOT fatal — the program runs "degraded" (rendering
// the grid but reporting that opens won't work) so the user can still see what
// is configured.
func Run(cfg Config) error {
	fl, err := loadCustomizations(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if !fl.FleetLaunch.Configured() {
		fmt.Println("Fleet Launch has nothing to show: no links (fleetLaunch.sites) or apps (fleetLaunch.apps) are configured in this devcontainer.json.")
		return nil
	}

	// A dial failure is expected when the host fleet TUI isn't running; keep
	// the client nil and run degraded rather than aborting.
	client, dialErr := control.Dial(cfg.socketPath())
	if client != nil {
		defer client.Close()
	}

	m := newModel(buildItems(fl), client, dialErr)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, runErr := prog.Run()
	return runErr
}

// loadCustomizations reads the fleetLaunch configuration from an explicit path
// when one is given, otherwise auto-detects it from the current directory.
func loadCustomizations(configPath string) (devcontainer.FleetCustomizations, error) {
	if configPath != "" {
		return devcontainer.LoadFleetCustomizationsFromFile(configPath)
	}
	return devcontainer.LoadFleetCustomizations(".")
}
