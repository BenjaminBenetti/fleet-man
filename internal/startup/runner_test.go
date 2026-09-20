package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A body that leaves something RUNNING (the mic script starts a sound server)
// must not hang the caller: the host reads the exec's output until every writer
// has closed it, and fd 3 — the wrapper's handle on the host's stderr — would be
// inherited by whatever the body leaves behind.
func TestWrapDoesNotLetALeftoverProcessHoldTheCaller(t *testing.T) {
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		// 0/1/2 redirected, higher fds untouched: the ordinary daemonize shape.
		out, err = runWrapped(t, Script{Name: "demo", Body: "sleep 20 </dev/null >/dev/null 2>&1 &\necho started"})
		close(done)
	}()
	select {
	case <-done:
		if err != nil {
			t.Fatalf("err=%v out=%q", err, out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wrapper is still blocked on a background process it left behind")
	}
}

// A deadline has to hold INSIDE the instance too: killing the host-side exec
// does not stop an apt in the container, which would keep the dpkg lock.
func TestBoundedShellStopsTheScriptItself(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout(1) here")
	}
	marker := filepath.Join(t.TempDir(), "survived")
	argv := boundedShell("sleep 3; touch "+marker, time.Second)
	start := time.Now()
	_ = exec.Command(argv[0], argv[1:]...).Run()
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("the script ran for %s past a 1 s deadline", elapsed)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the script carried on after its deadline")
	}
	if got := boundedShell("x", 0); len(got) != 3 || got[2] != "x" {
		t.Fatalf("no timeout should mean a plain sh -c: %v", got)
	}
}
