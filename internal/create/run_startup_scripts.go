package create

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// runStartupScripts loads the fleet's settings, picks the matching
// install scripts (Claude Code, Codex, …, plus audio packages when microphone
// input or audio output is enabled), and runs each one inside
// the container. Output is captured to ~/.fleet/startup/<name>.log
// inside the instance; per-script failures are aggregated into a
// warning file so the TUI can surface them without marking the
// instance as failed.
//
// State load failures are silently tolerated: if state cannot be read,
// no scripts are run. The caller proceeds to mark the instance running
// regardless — install can be re-attempted by the user via shell after
// the instance comes up.
func runStartupScripts(instanceBackend backend.Backend, wsDir, fleetName, instanceName string) {
	st, err := state.Load()
	if err != nil {
		return
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return
	}
	config, _ := state.LoadConfig()
	scripts := scriptsForInstance(instanceBackend, f.Settings, config)
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

// scriptsForInstance adds the audio stack when either global audio setting is
// enabled and supported by the backend. Provision at creation so applications
// can discover their virtual devices before a playback client attaches.
// These global settings are not part of startup.ScriptsFor's per-fleet toggles.
func scriptsForInstance(instanceBackend backend.Backend, settings fleet.FleetSettings, config *state.Config) []startup.Script {
	scripts := startup.ScriptsFor(settings)
	if config != nil {
		_, supportsOutput := instanceBackend.(backend.OutputBackend)
		mic := config.MicSettings.Enabled && instanceBackend.SupportsMicSink()
		output := config.OutputSettings.Enabled && supportsOutput
		if mic || output {
			scripts = append(scripts, startup.AudioScript(mic, output))
		}
	}
	return scripts
}
