package platform

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestOpenCommandCarriesTarget confirms the platform opener receives the target
// as its own argument (never shell-interpolated) on every supported OS.
func TestOpenCommandCarriesTarget(t *testing.T) {
	const target = "/tmp/chart with spaces.png"
	cmd, err := OpenCommand(target)
	if err != nil {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			t.Fatalf("unexpected error on %s: %v", runtime.GOOS, err)
		}
		return
	}
	carried := false
	for _, a := range cmd.Args {
		if a == target {
			carried = true
		}
	}
	for _, e := range cmd.Env {
		if strings.HasSuffix(e, "="+target) {
			carried = true
		}
	}
	if !carried {
		t.Errorf("opener should carry the target verbatim: args=%v", cmd.Args)
	}
}

// TestOpenFileCommandOverride covers FLEET_OPENER: the program (plus its own
// arguments) replaces the platform opener and the path is appended last.
func TestOpenFileCommandOverride(t *testing.T) {
	t.Setenv(EnvOpener, "  my-viewer --fullscreen ")
	cmd, err := OpenFileCommand("/tmp/x.png")
	if err != nil {
		t.Fatalf("OpenFileCommand: %v", err)
	}
	want := []string{"my-viewer", "--fullscreen", "/tmp/x.png"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestRunDetached covers the three outcomes: a prompt clean exit, a prompt
// failure (reported, with stderr), and a long-lived child that is left running
// after the grace period without holding the caller.
func TestRunDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	if err := runDetached(exec.Command("sh", "-c", "exit 0"), time.Second); err != nil {
		t.Errorf("clean exit: unexpected error %v", err)
	}
	err := runDetached(exec.Command("sh", "-c", "echo no handler >&2; exit 3"), time.Second)
	if err == nil || !strings.Contains(err.Error(), "no handler") || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("failure should report the exit status and stderr, got %v", err)
	}
	start := time.Now()
	if err := runDetached(exec.Command("sh", "-c", "sleep 5"), 100*time.Millisecond); err != nil {
		t.Errorf("long-lived child: unexpected error %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("long-lived child held the caller for %v", elapsed)
	}
	if _, err := exec.LookPath("no-such-opener-xyz"); err == nil {
		t.Skip("improbable: no-such-opener-xyz exists")
	}
	if err := runDetached(exec.Command("no-such-opener-xyz", "/tmp/x"), time.Second); err == nil {
		t.Errorf("missing program should fail to start")
	}
}

// TestCappedBuffer confirms the stderr keeper stops at its limit but keeps
// accepting (draining) writes.
func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{limit: 5}
	for _, chunk := range []string{"abc", "defg", "h"} {
		if n, err := b.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = (%d,%v), want (%d,nil)", chunk, n, err, len(chunk))
		}
	}
	if got := b.String(); got != "abcde" {
		t.Errorf("kept %q, want %q", got, "abcde")
	}
}
