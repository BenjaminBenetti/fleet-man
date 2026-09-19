package startup

import (
	"os/exec"
	"strings"
	"testing"
)

func runWrapped(t *testing.T, script Script) (string, error) {
	t.Helper()
	return runWrappedIn(t, t.TempDir(), script)
}

func runWrappedIn(t *testing.T, home string, script Script) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-c", wrap(script))
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A failing script's closing lines reach the host, so the warning the user sees
// says why — not just "exit status 3" with the diagnosis left in the container.
func TestWrapEchoesTheLogTailOnFailure(t *testing.T) {
	out, err := runWrapped(t, Script{Name: "demo", Body: "echo 'step one ok'\necho 'WARNING: the thing is misconfigured'\nexit 3"})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("the body's exit code must survive the wrapper: %v", err)
	}
	if !strings.Contains(out, "WARNING: the thing is misconfigured") {
		t.Fatalf("the diagnosis did not reach the host: %q", out)
	}
	if strings.Contains(out, "=== demo") {
		t.Fatalf("the run header is noise in a warning: %q", out)
	}
}

// …and a successful one stays silent: its output belongs in the log only.
func TestWrapIsSilentOnSuccess(t *testing.T) {
	out, err := runWrapped(t, Script{Name: "demo", Body: "echo 'lots of install chatter'\nexit 0"})
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

// A body that sets its own EXIT trap (codex does) must still run it, and must
// not swallow the wrapper's failure report.
func TestWrapSurvivesABodyWithItsOwnExitTrap(t *testing.T) {
	out, err := runWrapped(t, Script{Name: "demo", Body: "trap 'echo cleanup ran' EXIT\necho 'boom'\nexit 1"})
	if err == nil {
		t.Fatal("exit code lost")
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "cleanup ran") {
		t.Fatalf("out = %q", out)
	}
}

// The log is APPENDED to across runs. A short failing run must report its own
// lines only: padded with the previous run's, every retry's error text differs,
// which defeats de-duplication of the warning built from it.
func TestWrapTailIsScopedToTheCurrentRun(t *testing.T) {
	home := t.TempDir()
	if _, err := runWrappedIn(t, home, Script{Name: "demo", Body: "echo 'apt chatter 1'\necho 'apt chatter 2'\necho 'apt chatter 3'\nexit 1"}); err == nil {
		t.Fatal("run 1 should fail")
	}
	second, err := runWrappedIn(t, home, Script{Name: "demo", Body: "echo 'only this line'\nexit 1"})
	if err == nil {
		t.Fatal("run 2 should fail")
	}
	if strings.TrimSpace(second) != "only this line" {
		t.Fatalf("run 2 reported %q; the previous run's lines bled in", second)
	}
	third, _ := runWrappedIn(t, home, Script{Name: "demo", Body: "echo 'only this line'\nexit 1"})
	if third != second {
		t.Fatalf("identical failures must produce identical text: %q vs %q", second, third)
	}
}
