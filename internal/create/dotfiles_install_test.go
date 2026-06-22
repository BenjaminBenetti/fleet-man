package create

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeExecer is a minimal dotfilesExecer that records the commands it is asked
// to build and returns a backend.Cmd produced by make(attempt). attempt is
// 1-based so tests can vary behaviour per try (e.g. hang then succeed).
type fakeExecer struct {
	scripts []string // the `sh -c <script>` argument captured per call
	cmdFor  func(attempt int) *backend.Cmd
}

func (f *fakeExecer) ExecCommand(_ string, command []string) *backend.Cmd {
	if len(command) == 3 {
		f.scripts = append(f.scripts, command[2])
	}
	return f.cmdFor(len(f.scripts))
}

func sleepCmd() *backend.Cmd { return backend.NewCmd(exec.Command("sh", "-c", "sleep 30"), nil) }
func okCmd() *backend.Cmd    { return backend.NewCmd(exec.Command("sh", "-c", "exit 0"), nil) }
func failCmd() *backend.Cmd  { return backend.NewCmd(exec.Command("sh", "-c", "exit 1"), nil) }

// withShortDotfilesKnobs shrinks the package-level timeout/attempts for the
// duration of a test and restores them afterwards, so the retry loop runs in
// milliseconds without containers.
func withShortDotfilesKnobs(t *testing.T, timeout time.Duration, attempts int) {
	t.Helper()
	origTimeout, origAttempts := dotfilesInstallTimeout, dotfilesInstallAttempts
	dotfilesInstallTimeout, dotfilesInstallAttempts = timeout, attempts
	t.Cleanup(func() {
		dotfilesInstallTimeout, dotfilesInstallAttempts = origTimeout, origAttempts
	})
}

// TestInstallDotfilesRetriesOnHangThenWarns verifies the headline requirement:
// a dotfiles install that hangs is killed after the timeout and retried up to
// the attempt limit, after which a single warning is written and the caller
// returns (so the instance starts anyway).
func TestInstallDotfilesRetriesOnHangThenWarns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withShortDotfilesKnobs(t, 50*time.Millisecond, 3)

	fe := &fakeExecer{cmdFor: func(int) *backend.Cmd { return sleepCmd() }}
	start := time.Now()
	installDotfiles(fe, "fleetx", "inst1", "/ws", "echo install")
	elapsed := time.Since(start)

	if len(fe.scripts) != 3 {
		t.Fatalf("install attempted %d times, want 3", len(fe.scripts))
	}
	// 3 attempts × 50ms ≈ 150ms of deadlines; well under the 30s sleeps.
	if elapsed > 15*time.Second {
		t.Errorf("installDotfiles took %s; expected it to bail out via timeouts", elapsed)
	}

	warn, err := os.ReadFile(state.WarnPath("fleetx", "inst1"))
	if err != nil {
		t.Fatalf("expected a warning file after all attempts failed: %v", err)
	}
	if !strings.Contains(string(warn), "after 3 attempt") {
		t.Errorf("warning = %q, want it to mention the attempt count", string(warn))
	}
}

// TestInstallDotfilesSucceedsFirstTry verifies the common path: one successful
// attempt, no retry, no warning, and no destructive rm-rf prefix.
func TestInstallDotfilesSucceedsFirstTry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withShortDotfilesKnobs(t, 5*time.Second, 3)

	fe := &fakeExecer{cmdFor: func(int) *backend.Cmd { return okCmd() }}
	installDotfiles(fe, "fleetx", "inst1", "/ws", "echo install")

	if len(fe.scripts) != 1 {
		t.Fatalf("install attempted %d times, want 1", len(fe.scripts))
	}
	if strings.Contains(fe.scripts[0], "rm -rf") {
		t.Errorf("first attempt should not clear ~/dotfiles, got script %q", fe.scripts[0])
	}
	if _, err := os.Stat(state.WarnPath("fleetx", "inst1")); !os.IsNotExist(err) {
		t.Errorf("expected no warning file on success, stat err = %v", err)
	}
}

// TestInstallDotfilesRecoversOnRetry verifies that a transient failure is
// retried and that the retry clears a possibly-partial ~/dotfiles before
// re-running, then a later success stops the loop with no warning.
func TestInstallDotfilesRecoversOnRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withShortDotfilesKnobs(t, 5*time.Second, 3)

	fe := &fakeExecer{cmdFor: func(attempt int) *backend.Cmd {
		if attempt == 1 {
			return failCmd()
		}
		return okCmd()
	}}
	installDotfiles(fe, "fleetx", "inst1", "/ws", "echo install")

	if len(fe.scripts) != 2 {
		t.Fatalf("install attempted %d times, want 2 (fail then succeed)", len(fe.scripts))
	}
	if strings.Contains(fe.scripts[0], "rm -rf ~/dotfiles") {
		t.Errorf("first attempt must not clear ~/dotfiles, got %q", fe.scripts[0])
	}
	if !strings.HasPrefix(fe.scripts[1], "rm -rf ~/dotfiles;") {
		t.Errorf("retry must clear a partial ~/dotfiles first, got %q", fe.scripts[1])
	}
	if _, err := os.Stat(state.WarnPath("fleetx", "inst1")); !os.IsNotExist(err) {
		t.Errorf("expected no warning file once a retry succeeds, stat err = %v", err)
	}
}
