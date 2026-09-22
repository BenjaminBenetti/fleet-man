package create

import (
	"slices"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// micCapability is a backend.Backend that only answers SupportsMicSink.
type micCapability struct {
	backend.Backend
	supports bool
}

func (m micCapability) SupportsMicSink() bool { return m.supports }

func scriptNames(scripts []startup.Script) []string {
	var names []string
	for _, script := range scripts {
		names = append(names, script.Name)
	}
	return names
}

// The audio stack is installed only when the global microphone setting is on
// AND the backend can host a sink — never as a side effect of a fleet toggle,
// and never for a sound server nothing could feed.
func TestScriptsForInstanceGatesTheMicScript(t *testing.T) {
	on := &state.Config{MicSettings: state.MicSettings{Enabled: true}}
	off := &state.Config{}
	settings := fleet.FleetSettings{ClaudeCodeMount: true}

	for name, tc := range map[string]struct {
		config   *state.Config
		supports bool
		want     []string
	}{
		"enabled + capable backend":    {on, true, []string{"claude-code", "mic"}},
		"enabled, backend has no sink": {on, false, []string{"claude-code"}},
		"disabled":                     {off, true, []string{"claude-code"}},
		"no config":                    {nil, true, []string{"claude-code"}},
	} {
		got := scriptNames(scriptsForInstance(micCapability{supports: tc.supports}, settings, tc.config))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: scripts = %v, want %v", name, got, tc.want)
		}
	}

	// The mic script goes LAST: agent installs must not wait behind apt.
	if got := scriptNames(scriptsForInstance(micCapability{supports: true}, fleet.FleetSettings{}, on)); !slices.Equal(got, []string{"mic"}) {
		t.Errorf("mic only: %v", got)
	}
}
