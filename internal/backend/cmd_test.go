package backend

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCombinedOutputWithTimeoutFast verifies the happy path: a command that
// finishes well inside the deadline returns its combined output with no error,
// and onDone fires exactly once.
func TestCombinedOutputWithTimeoutFast(t *testing.T) {
	var fired int
	cmd := NewCmd(exec.Command("sh", "-c", "echo hello; echo oops 1>&2"), func(time.Duration, error) {
		fired++
	})

	out, err := cmd.CombinedOutputWithTimeout(5 * time.Second)
	if err != nil {
		t.Fatalf("CombinedOutputWithTimeout returned error: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "oops") {
		t.Errorf("combined output = %q, want both stdout and stderr captured", got)
	}
	if fired != 1 {
		t.Errorf("onDone fired %d times, want 1", fired)
	}
}

// TestCombinedOutputWithTimeoutKills verifies that a command which overruns the
// deadline is killed and reported as a timeout, and crucially that the call
// returns promptly (≈ the timeout, not the command's full sleep) so a hung
// install can never stall the caller.
func TestCombinedOutputWithTimeoutKills(t *testing.T) {
	var fired int
	cmd := NewCmd(exec.Command("sh", "-c", "sleep 30"), func(time.Duration, error) {
		fired++
	})

	start := time.Now()
	_, err := cmd.CombinedOutputWithTimeout(100 * time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("CombinedOutputWithTimeout returned nil error for a hung command")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout error", err)
	}
	// Allow generous slack for the WaitDelay-bounded pipe drain, but it must be
	// far short of the command's own 30s sleep.
	if elapsed > 10*time.Second {
		t.Errorf("CombinedOutputWithTimeout took %s, expected it to return shortly after the 100ms deadline", elapsed)
	}
	if fired != 1 {
		t.Errorf("onDone fired %d times, want 1", fired)
	}
}

// TestCombinedOutputWithTimeoutZeroNoDeadline verifies that a non-positive
// timeout defers to the plain CombinedOutput (no deadline), still capturing
// output and firing onDone once.
func TestCombinedOutputWithTimeoutZeroNoDeadline(t *testing.T) {
	var fired int
	cmd := NewCmd(exec.Command("sh", "-c", "echo ok"), func(time.Duration, error) {
		fired++
	})

	out, err := cmd.CombinedOutputWithTimeout(0)
	if err != nil {
		t.Fatalf("CombinedOutputWithTimeout(0) returned error: %v", err)
	}
	if !strings.Contains(string(out), "ok") {
		t.Errorf("combined output = %q, want it to contain %q", string(out), "ok")
	}
	if fired != 1 {
		t.Errorf("onDone fired %d times, want 1", fired)
	}
}
