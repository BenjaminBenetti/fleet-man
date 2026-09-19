package startup

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
)

func TestMicScriptInstallsAndConfiguresTheAudioStack(t *testing.T) {
	script := MicScript()
	if script.Name != "mic" {
		t.Fatalf("Name = %q", script.Name)
	}
	for _, want := range []string{
		// every supported package manager gets the server, pactl, the ALSA pulse plugin and arecord
		"pulseaudio pulseaudio-utils libasound2-plugins alsa-utils",
		"pulseaudio pulseaudio-utils alsa-plugins-pulse alsa-utils",
		"pulseaudio pulseaudio-utils alsa-plugins-pulseaudio alsa-utils",
		// the ALSA default becomes the virtual microphone
		"pcm.!default { type pulse }",
		// pulse clients find the private server without PULSE_SERVER
		"socket='" + micsink.SocketPath + "'",
		"default-server = unix:$socket",
		"autospawn = no",
		"fleet mic ensure",
		"sudo -n",
	} {
		if !strings.Contains(script.Body, want) {
			t.Errorf("mic script missing %q", want)
		}
	}
}

// The script is shell assembled with Sprintf; make sure what comes out parses.
func TestMicScriptIsValidShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(wrap(MicScript()))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// The microphone is a global setting: no FleetSettings toggle may pull it in.
func TestScriptsForNeverIncludesMic(t *testing.T) {
	all := fleet.FleetSettings{ClaudeCodeMount: true, CodexMount: true, AuggieMount: true}
	for _, script := range ScriptsFor(all) {
		if script.Name == "mic" {
			t.Fatal("ScriptsFor must not return the mic script")
		}
	}
}
