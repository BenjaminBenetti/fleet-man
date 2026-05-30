package instanceops

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// postStartHook runs after a successful container Start to set up
// in-container scaffolding: the fleet-launch binary so later
// subcommands have something to invoke, and the fleet.rc so
// interactive shells can source fleet-aware aliases. homeDir is the
// fleet's persisted HomeDir setting (empty falls back to
// fleetlaunch.DefaultHomeDir). Failures on either step are surfaced
// as warnings rather than failing the start; the browser-open path
// stages the binary on demand as a backstop.
//
// It is a package-level var so tests can replace it with a no-op
// (a real call would need a host backend + a real container).
var postStartHook = func(fleetName string, instance *fleet.Instance, homeDir string) {
	instanceBackend := backendutil.NewForInstance(instance, false)
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, instance.WorkspaceDir, nil); err != nil {
		state.WriteWarn(fleetName, instance.Name, fmt.Sprintf("stage fleet-launch: %v", err))
	}
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, instance.WorkspaceDir, homeDir); err != nil {
		state.WriteWarn(fleetName, instance.Name, fmt.Sprintf("stage fleet.rc: %v", err))
	}
}
