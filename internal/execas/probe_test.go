package execas

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

func TestProbeArgsMatchTheCLI(t *testing.T) {
	for _, tc := range []struct {
		mode  ProbeMode
		shell string
		want  []string
	}{
		{ProbeLoginInteractiveShell, "/bin/bash", []string{"/bin/bash", "-lic", "S"}},
		{ProbeLoginShell, "/bin/zsh", []string{"/bin/zsh", "-lc", "S"}},
		{ProbeInteractiveShell, "/bin/sh", []string{"/bin/sh", "-ic", "S"}},
		{ProbeLoginInteractiveShell, "/usr/bin/pwsh", []string{"/usr/bin/pwsh", "-Login", "-Command", "S"}},
		{ProbeLoginShell, "/opt/pwsh-preview", []string{"/opt/pwsh-preview", "-Login", "-Command", "S"}},
		{ProbeInteractiveShell, "/usr/bin/pwsh", []string{"/usr/bin/pwsh", "-Command", "S"}},
	} {
		if got := probeArgs(tc.mode, tc.shell, "S"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("probeArgs(%s, %s) = %v, want %v", tc.mode, tc.shell, got, tc.want)
		}
	}
	if got, want := probeScript("M", probeDumpProc), "echo -n M; cat /proc/self/environ; echo -n M"; got != want {
		t.Fatalf("probeScript = %q, want %q", got, want)
	}
}

func TestParseProbeOutput(t *testing.T) {
	out := []byte("motd from .bashrc\nMARKA=1\x00B=two=parts\x00PWD=/probe\x00noequals\x00=nameless\x00A=later\x00EMPTY=\x00MARKtrailing")
	got, ok := parseProbeOutput(out, "MARK", 0)
	if !ok {
		t.Fatal("marks not found")
	}
	want := map[string]string{"A": "later", "B": "two=parts", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	got, ok = parseProbeOutput([]byte("MARKPATH=/bin\nHOME=/h\nMARK"), "MARK", '\n')
	if !ok || !reflect.DeepEqual(got, map[string]string{"PATH": "/bin", "HOME": "/h"}) {
		t.Fatalf("printenv form: %v, %v", got, ok)
	}

	for _, out := range []string{"", "no marks", "MARK only one", "MARKMARK"} {
		if _, ok := parseProbeOutput([]byte(out), "MARK", 0); ok {
			t.Errorf("%q parsed", out)
		}
	}
}

// TestDefaultProbeDepsAreTheSocketsShellsTest: every in-pod socket fleet.rc
// tests is a probe dep (a cached environment probed without it must not
// outlive its coming), and fleet.sock is not: it comes and goes with every
// TUI attach and detach, which would have each pod probe its user's
// environment again every time.
func TestDefaultProbeDepsAreTheSocketsShellsTest(t *testing.T) {
	rc, err := os.ReadFile(filepath.Join("..", "fleetlaunch", "fleet.rc"))
	if err != nil {
		t.Fatal(err)
	}
	deps := map[string]bool{}
	for _, d := range DefaultProbeDeps {
		deps[d] = true
	}
	tested := regexp.MustCompile(`-[Se] (/fleet-mounts/control/[^ ;\]]+)`).FindAllStringSubmatch(string(rc), -1)
	if len(tested) == 0 {
		t.Fatal("fleet.rc tests no in-pod socket: the pattern is stale")
	}
	for _, m := range tested {
		if !deps[m[1]] {
			t.Errorf("fleet.rc tests %s, which is not in DefaultProbeDeps", m[1])
		}
	}
	if deps["/fleet-mounts/control/fleet.sock"] {
		t.Errorf("DefaultProbeDeps holds fleet.sock, which every TUI attach and detach binds and unbinds")
	}
}
