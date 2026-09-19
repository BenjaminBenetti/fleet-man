package create

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// runStartupScripts loads the fleet's settings, picks the matching
// install scripts (Claude Code, Codex, …, plus the virtual microphone's
// audio packages when that global setting is on), and runs each one inside
// the container. Output is captured to ~/.fleet/startup/<name>.log
// inside the instance; per-script failures are aggregated into a
// warning file so the TUI can surface them without marking the
// instance as failed.
//
// State load failures are silently tolerated: if state cannot be read,
// no scripts are run. The caller proceeds to mark the instance running
// regardless — install can be re-attempted by the user via shell after
// the instance comes up.
func runStartupScripts(instanceBackend backend.Backend, wsDir, fleetName, instanceName, containerID string) {
	st, err := state.Load()
	if err != nil {
		return
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return
	}
	scripts := startup.ScriptsFor(f.Settings)
	// The virtual microphone is a global setting rather than a fleet toggle, so
	// it is not part of ScriptsFor. Only backends the daemon can attach a sink to
	// get the packages — installing a sound server nothing will feed is waste.
	if config, _ := state.LoadConfig(); config != nil && config.MicSettings.Enabled {
		if _, ok := instanceBackend.MicSinkCommand(containerID); ok {
			scripts = append(scripts, startup.MicScript())
		}
	}
	if len(scripts) == 0 {
		return
	}
	failures := startup.Run(instanceBackend, wsDir, scripts)
	if len(failures) == 0 {
		return
	}
	lines := make([]string, 0, len(failures))
	for _, failure := range failures {
		lines = append(lines, failure.Error())
	}
	state.WriteWarn(fleetName, instanceName, strings.Join(lines, "\n"))
}
