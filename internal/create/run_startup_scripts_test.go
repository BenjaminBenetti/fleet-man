package create

import (
	"os/exec"
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

type outputCapability struct{ micCapability }

func (outputCapability) OutputSourceCommand(string) (*exec.Cmd, bool) { return nil, true }

func TestScriptsForInstanceGatesOutputIndependently(t *testing.T) {
	config := state.DefaultConfig()
	if got := scriptNames(scriptsForInstance(micCapability{supports: true}, fleet.FleetSettings{}, config)); len(got) != 0 {
		t.Fatalf("unsupported backend installs output: %v", got)
	}
	b := outputCapability{micCapability{supports: true}}
	if got := scriptNames(scriptsForInstance(b, fleet.FleetSettings{}, config)); !slices.Equal(got, []string{"output"}) {
		t.Fatalf("output-only scripts: %v", got)
	}
	config.OutputSettings.Enabled = false
	if got := scriptNames(scriptsForInstance(b, fleet.FleetSettings{}, config)); len(got) != 0 {
		t.Fatalf("disabled output installs audio: %v", got)
	}
	config.OutputSettings.Enabled = true
	config.MicSettings.Enabled = true
	scripts := scriptsForInstance(b, fleet.FleetSettings{}, config)
	if len(scripts) != 1 {
		t.Fatalf("both directions should install the stack once: %v", scriptNames(scripts))
	}
}

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

// gatedAudio is an output-capable backend whose instance can have no audio
// (backend.AudioGate).
type gatedAudio struct {
	outputCapability
	reason string
}

func (g gatedAudio) AudioUnavailable() string { return g.reason }

// An instance its backend says can have no audio gets no audio script, with
// either direction on; one it says can, the usual one.
func TestScriptsForInstanceSkipsAudioTheBackendCannotHave(t *testing.T) {
	config := &state.Config{MicSettings: state.MicSettings{Enabled: true}, OutputSettings: state.OutputSettings{Enabled: true}}
	settings := fleet.FleetSettings{ClaudeCodeMount: true}
	gated := gatedAudio{outputCapability{micCapability{supports: true}}, "audio unavailable on cluster c: containers can't run as root"}
	if got := scriptNames(scriptsForInstance(gated, settings, config)); !slices.Equal(got, []string{"claude-code"}) {
		t.Fatalf("an audio-less instance got %v", got)
	}
	gated.reason = ""
	if got := scriptNames(scriptsForInstance(gated, settings, config)); !slices.Equal(got, []string{"claude-code", "mic"}) {
		t.Fatalf("an audio-capable gated backend got %v", got)
	}
}
