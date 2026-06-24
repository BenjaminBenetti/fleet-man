package tui

import (
	"errors"
	"strings"
	"testing"
)

// runSessionScript must fold a non-zero tmux exit into an error that CARRIES the
// captured reason (create scripts merge stderr via 2>&1), so the TUI shows
// "duplicate session: NAME" instead of a bare "exit status 1".
func TestRunSessionScriptSurfacesReason(t *testing.T) {
	orig := runInstanceCommand
	defer func() { runInstanceCommand = orig }()

	runInstanceCommand = func(_, _ string, _ []string) (string, int, error) {
		return "duplicate session: inst~dev\n", 1, nil
	}
	_, err := runSessionScript(InstanceRef{Fleet: "f", Instance: "inst"}, "ignored")
	if err == nil {
		t.Fatal("expected an error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "duplicate session: inst~dev") {
		t.Fatalf("error dropped the reason: %v", err)
	}
}

// The TmuxEnsureInstalled "==> Installing tmux..." preamble (now on stdout via
// 2>&1) must not bury the real reason: only the last non-empty line is surfaced.
func TestRunSessionScriptDropsInstallPreamble(t *testing.T) {
	orig := runInstanceCommand
	defer func() { runInstanceCommand = orig }()

	runInstanceCommand = func(_, _ string, _ []string) (string, int, error) {
		return "==> Installing tmux...\nduplicate session: inst~dev\n", 1, nil
	}
	_, err := runSessionScript(InstanceRef{Fleet: "f", Instance: "inst"}, "ignored")
	if err == nil || !strings.Contains(err.Error(), "duplicate session: inst~dev") {
		t.Fatalf("reason not surfaced: %v", err)
	}
	if strings.Contains(err.Error(), "Installing tmux") {
		t.Fatalf("install preamble leaked into error: %v", err)
	}
}

// With no captured output the error is still the plain exit code (e.g. rename /
// delete scripts that keep 2>/dev/null), not "exit status 1: ".
func TestRunSessionScriptBareExitWhenSilent(t *testing.T) {
	orig := runInstanceCommand
	defer func() { runInstanceCommand = orig }()

	runInstanceCommand = func(_, _ string, _ []string) (string, int, error) {
		return "", 1, nil
	}
	_, err := runSessionScript(InstanceRef{Fleet: "f", Instance: "inst"}, "ignored")
	if err == nil || err.Error() != "exit status 1" {
		t.Fatalf("got %v, want bare \"exit status 1\"", err)
	}
}

// A transport/start failure (err != nil) is returned verbatim, untouched by the
// exit-code folding.
func TestRunSessionScriptPropagatesTransportError(t *testing.T) {
	orig := runInstanceCommand
	defer func() { runInstanceCommand = orig }()

	boom := errors.New("dial: connection refused")
	runInstanceCommand = func(_, _ string, _ []string) (string, int, error) {
		return "", 0, boom
	}
	if _, err := runSessionScript(InstanceRef{Fleet: "f", Instance: "inst"}, "ignored"); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the transport error", err)
	}
}
