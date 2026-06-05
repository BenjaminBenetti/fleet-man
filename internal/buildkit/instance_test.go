package buildkit

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// fakeExecer satisfies instanceExecer. It records the joined commands it is
// asked to run and returns canned stdout: the probe (detected by the
// "buildx version" substring) yields probeOut; every other command succeeds
// with empty output. Backing each call with a real `printf` keeps the
// *backend.Cmd run methods working exactly as in production.
type fakeExecer struct {
	probeOut string
	calls    []string
}

func (f *fakeExecer) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	joined := strings.Join(command, " ")
	f.calls = append(f.calls, joined)
	out := ""
	if strings.Contains(joined, "buildx version") {
		out = f.probeOut
	}
	return backend.NewCmd(exec.Command("printf", "%s", out), nil)
}

func TestBuildxPresent(t *testing.T) {
	cases := map[string]bool{
		"PRESENT":            true,
		"PRESENT\n":          true,
		"warning\nPRESENT\n": true,
		"ABSENT":             false,
		"ABSENT\n":           false,
		"":                   false,
	}
	for in, want := range cases {
		if got := buildxPresent(in); got != want {
			t.Errorf("buildxPresent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProbeAndConfigureScripts(t *testing.T) {
	probe := probeScript()
	for _, want := range []string{"command -v docker", "docker buildx version", probeMarkerPresent, probeMarkerAbsent} {
		if !strings.Contains(probe, want) {
			t.Errorf("probeScript missing %q: %s", want, probe)
		}
	}
	cfg := configureScript()
	for _, want := range []string{
		"docker buildx rm " + builderName,
		"docker buildx create --name " + builderName,
		"--driver remote",
		"--use unix://" + containerSocketPath,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configureScript missing %q: %s", want, cfg)
		}
	}
}

func TestConfigureInstanceBuildxSkipsWithoutDocker(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerAbsent}
	if err := ConfigureInstanceBuildx(fe, "/ws"); err != nil {
		t.Fatalf("ConfigureInstanceBuildx (absent) = %v, want nil", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only the probe call, got %d: %v", len(fe.calls), fe.calls)
	}
	for _, c := range fe.calls {
		if strings.Contains(c, "buildx create") {
			t.Fatalf("must NOT configure buildx when docker/buildx absent: %v", fe.calls)
		}
	}
}

func TestConfigureInstanceBuildxConfiguresWhenPresent(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerPresent}
	if err := ConfigureInstanceBuildx(fe, "/ws"); err != nil {
		t.Fatalf("ConfigureInstanceBuildx (present) = %v, want nil", err)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("expected probe + configure, got %d: %v", len(fe.calls), fe.calls)
	}
	if !strings.Contains(fe.calls[1], "buildx create --name "+builderName) {
		t.Fatalf("configure call did not create the shared builder: %v", fe.calls[1])
	}
}
