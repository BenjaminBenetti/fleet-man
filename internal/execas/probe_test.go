package execas

import (
	"reflect"
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
