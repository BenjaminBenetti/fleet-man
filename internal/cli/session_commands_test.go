package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/spf13/cobra"
)

// ============================================================================
// Test helpers
// ============================================================================

// seedState writes a single-fleet, single-instance state to a temp HOME and
// returns the "fleet/instance" arg the commands accept. status controls
// whether the instance appears running or stopped.
func seedState(t *testing.T, status fleet.InstanceStatus) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	const (
		fleetName    = "tf"
		instanceName = "agent-1"
	)
	st := &state.State{
		Fleets: map[string]*fleet.Fleet{
			fleetName: {
				Name: fleetName,
				Instances: []*fleet.Instance{
					{
						Name:         instanceName,
						ContainerID:  "fake-container-id",
						WorkspaceDir: "/tmp/fake",
						CreatedAt:    time.Unix(0, 0),
						Status:       status,
					},
				},
			},
		},
	}
	if err := state.Save(st); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	return fleetName + "/" + instanceName
}

// tmuxIsolatedEnv returns a copy of os.Environ with all tmux-related vars
// stripped, then TMUX_TMPDIR pinned to tmuxDir. This is critical: when these
// tests run inside an actual tmux session (e.g. when developing fleet from
// fleet), an inherited TMUX env var would cause child tmux invocations to
// target the developer's running server instead of our private socket. The
// cleanup's `tmux kill-server` would then murder the developer's session.
func tmuxIsolatedEnv(tmuxDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "TMUX="),
			strings.HasPrefix(kv, "TMUX_PANE="),
			strings.HasPrefix(kv, "TMUX_TMPDIR="):
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TMUX_TMPDIR="+tmuxDir)
}

// useHostTmux skips the test if tmux is missing on PATH, then redirects
// sessionExecCommand so commands run on the host against a private tmux
// socket. Returns the TMUX_TMPDIR; cleanup is registered with t.Cleanup.
func useHostTmux(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed on host; skipping integration test")
	}
	tmuxDir := t.TempDir()

	prevExec := sessionExecCommand
	sessionExecCommand = func(_, _ string, args []string) (*exec.Cmd, error) {
		c := exec.Command(args[0], args[1:]...)
		c.Env = tmuxIsolatedEnv(tmuxDir)
		return c, nil
	}
	t.Cleanup(func() {
		sessionExecCommand = prevExec
		kill := exec.Command("tmux", "kill-server")
		kill.Env = tmuxIsolatedEnv(tmuxDir)
		_ = kill.Run()
	})
	return tmuxDir
}

// hostTmux invokes the host tmux binary against the test's private socket
// dir and returns combined stdout+stderr plus any error.
func hostTmux(t *testing.T, tmuxDir string, args ...string) (string, error) {
	t.Helper()
	c := exec.Command("tmux", args...)
	c.Env = tmuxIsolatedEnv(tmuxDir)
	out, err := c.CombinedOutput()
	return string(out), err
}

// run executes a cobra command in this package with the given args via the
// full Execute() pipeline so flag parsing fires.
func run(t *testing.T, c *cobra.Command, args ...string) error {
	t.Helper()
	c.SetArgs(args)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c.Execute()
}

// waitForFile polls until the path exists or the deadline passes.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for sentinel file %s", path)
}

// ============================================================================
// Error-path tests — do not require tmux on the host.
// ============================================================================

func TestSpawnSession_FleetNotFound(t *testing.T) {
	_ = seedState(t, fleet.StatusRunning)
	err := run(t, newSpawnSessionCmd(), "missing-fleet/agent-1", "demo")
	if err == nil || !strings.Contains(err.Error(), "missing-fleet") {
		t.Fatalf("expected fleet-not-found error, got %v", err)
	}
}

func TestSpawnSession_InstanceNotFound(t *testing.T) {
	_ = seedState(t, fleet.StatusRunning)
	err := run(t, newSpawnSessionCmd(), "tf/ghost", "demo")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected instance-not-found error, got %v", err)
	}
}

func TestSpawnSession_InstanceNotRunning(t *testing.T) {
	target := seedState(t, fleet.StatusStopped)
	err := run(t, newSpawnSessionCmd(), target, "demo")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not-running error, got %v", err)
	}
}

func TestExecInSession_InstanceNotRunning(t *testing.T) {
	target := seedState(t, fleet.StatusStopped)
	err := run(t, newExecInSessionCmd(), target, "demo", "echo", "hi")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not-running error, got %v", err)
	}
}

func TestReadSession_FleetNotFound(t *testing.T) {
	_ = seedState(t, fleet.StatusRunning)
	err := run(t, newReadSessionCmd(), "missing-fleet/agent-1", "demo")
	if err == nil || !strings.Contains(err.Error(), "missing-fleet") {
		t.Fatalf("expected fleet-not-found error, got %v", err)
	}
}

// ============================================================================
// Integration tests — drive real tmux on the host.
// ============================================================================

func TestSpawnSession_CreatesSession(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	tmuxDir := useHostTmux(t)

	if err := run(t, newSpawnSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("spawn-session returned error: %v", err)
	}

	out, err := hostTmux(t, tmuxDir, "ls")
	if err != nil {
		t.Fatalf("tmux ls failed: %v\n%s", err, out)
	}
	// spawn-session canonicalizes names to the TUI group convention
	// (<instance>~<name>) so the session shows up as a regular group.
	if !strings.Contains(out, "agent-1~demo:") {
		t.Fatalf("tmux ls did not list 'agent-1~demo' session:\n%s", out)
	}
}

func TestSpawnSession_AcceptsCanonicalName(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	tmuxDir := useHostTmux(t)

	// A name that already follows the <instance>~<group> convention must
	// pass through unchanged, not get double-prefixed.
	if err := run(t, newSpawnSessionCmd(), target, "agent-1~demo"); err != nil {
		t.Fatalf("spawn-session returned error: %v", err)
	}

	out, err := hostTmux(t, tmuxDir, "ls")
	if err != nil {
		t.Fatalf("tmux ls failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent-1~demo:") || strings.Contains(out, "agent-1~agent-1~") {
		t.Fatalf("expected exactly 'agent-1~demo', got:\n%s", out)
	}
}

func TestSpawnSession_ErrorOnNonexistentSessionFromExec(t *testing.T) {
	// Skip if no host tmux — the validation we want exercises real tmux.
	target := seedState(t, fleet.StatusRunning)
	_ = useHostTmux(t)

	err := run(t, newExecInSessionCmd(), target, "no-such-session", "echo", "x")
	if err == nil || !strings.Contains(err.Error(), "no-such-session") {
		t.Fatalf("expected wrapped error mentioning session name, got %v", err)
	}
}

func TestExecInSession_SendsCommandToShell(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	tmuxDir := useHostTmux(t)

	if err := run(t, newSpawnSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("spawn-session: %v", err)
	}

	// Use a sentinel file as the proof-of-execution: send a command that
	// touches a known path, then poll for it.
	sentinel := filepath.Join(t.TempDir(), "ran.txt")
	cmd := fmt.Sprintf("echo executed > %s", sentinel)
	if err := run(t, newExecInSessionCmd(), target, "demo", cmd); err != nil {
		t.Fatalf("exec-in-session: %v", err)
	}

	waitForFile(t, sentinel, 3*time.Second)

	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(body), "executed") {
		t.Fatalf("sentinel had unexpected content: %q", body)
	}

	_ = tmuxDir // useHostTmux registers cleanup; we don't need the dir directly here
}

func TestExecInSession_PreservesShellState(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	_ = useHostTmux(t)

	if err := run(t, newSpawnSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("spawn-session: %v", err)
	}

	// First call sets a variable; second call must see it — proving both
	// calls hit the same long-lived shell inside the session.
	if err := run(t, newExecInSessionCmd(), target, "demo", "export", "FLEET_TEST_VAR=persisted"); err != nil {
		t.Fatalf("exec-in-session set: %v", err)
	}

	sentinel := filepath.Join(t.TempDir(), "var.txt")
	if err := run(t, newExecInSessionCmd(), target, "demo",
		fmt.Sprintf("echo VAR=$FLEET_TEST_VAR > %s", sentinel)); err != nil {
		t.Fatalf("exec-in-session read: %v", err)
	}

	waitForFile(t, sentinel, 3*time.Second)
	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(body), "VAR=persisted") {
		t.Fatalf("expected persisted var, got %q", body)
	}
}

func TestReadSession_CapturesVisiblePane(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	_ = useHostTmux(t)

	if err := run(t, newSpawnSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("spawn-session: %v", err)
	}

	sentinel := filepath.Join(t.TempDir(), "ran.txt")
	cmd := fmt.Sprintf("echo CAPTURE_ME && touch %s", sentinel)
	if err := run(t, newExecInSessionCmd(), target, "demo", cmd); err != nil {
		t.Fatalf("exec-in-session: %v", err)
	}
	waitForFile(t, sentinel, 3*time.Second)

	var buf bytes.Buffer
	prevOut, prevErr := readSessionStdout, readSessionStderr
	readSessionStdout = &buf
	readSessionStderr = io.Discard
	defer func() { readSessionStdout, readSessionStderr = prevOut, prevErr }()

	if err := run(t, newReadSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("read-session: %v", err)
	}

	if !strings.Contains(buf.String(), "CAPTURE_ME") {
		t.Fatalf("captured pane missing expected output:\n%s", buf.String())
	}
}

// generateLinesAndWait writes a numbered series of lines to a session and
// blocks until the sentinel proves the last line ran. The sentinel is keyed
// off the loop's final iteration so we know all lines were emitted.
func generateLinesAndWait(t *testing.T, target string, n int) {
	t.Helper()
	sentinel := filepath.Join(t.TempDir(), "done.txt")
	cmd := fmt.Sprintf("for i in $(seq 1 %d); do echo line-$i; done && touch %s", n, sentinel)
	if err := run(t, newExecInSessionCmd(), target, "demo", cmd); err != nil {
		t.Fatalf("exec-in-session: %v", err)
	}
	waitForFile(t, sentinel, 5*time.Second)
}

func TestReadSession_ScrollbackN(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	_ = useHostTmux(t)

	if err := run(t, newSpawnSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("spawn-session: %v", err)
	}

	generateLinesAndWait(t, target, 200)

	var visible, with50 bytes.Buffer
	captureInto := func(buf *bytes.Buffer, args ...string) {
		buf.Reset()
		prevOut, prevErr := readSessionStdout, readSessionStderr
		readSessionStdout = buf
		readSessionStderr = io.Discard
		defer func() { readSessionStdout, readSessionStderr = prevOut, prevErr }()
		if err := run(t, newReadSessionCmd(), args...); err != nil {
			t.Fatalf("read-session %v: %v", args, err)
		}
	}

	captureInto(&visible, target, "demo")
	captureInto(&with50, target, "-s", "50", "demo")

	visibleLines := countLines(visible.String(), "line-")
	with50Lines := countLines(with50.String(), "line-")

	if visibleLines == 0 {
		t.Fatalf("visible-only capture had no numbered lines:\n%s", visible.String())
	}
	if with50Lines <= visibleLines {
		t.Fatalf("--scrollback 50 should include more lines than visible-only (visible=%d, with50=%d)",
			visibleLines, with50Lines)
	}
	if with50Lines != visibleLines+50 {
		t.Fatalf("--scrollback 50 should add exactly 50 lines; visible=%d, with50=%d",
			visibleLines, with50Lines)
	}
}

func TestReadSession_ScrollbackAll(t *testing.T) {
	target := seedState(t, fleet.StatusRunning)
	_ = useHostTmux(t)

	if err := run(t, newSpawnSessionCmd(), target, "demo"); err != nil {
		t.Fatalf("spawn-session: %v", err)
	}

	generateLinesAndWait(t, target, 200)

	var buf bytes.Buffer
	prevOut, prevErr := readSessionStdout, readSessionStderr
	readSessionStdout = &buf
	readSessionStderr = io.Discard
	defer func() { readSessionStdout, readSessionStderr = prevOut, prevErr }()

	if err := run(t, newReadSessionCmd(), target, "-s", "-1", "demo"); err != nil {
		t.Fatalf("read-session: %v", err)
	}

	if countLines(buf.String(), "line-") != 200 {
		t.Fatalf("--scrollback -1 should capture all 200 lines; got %d\n%s",
			countLines(buf.String(), "line-"), buf.String())
	}
	if !strings.Contains(buf.String(), "line-1\n") {
		t.Fatalf("--scrollback -1 should include the first line (line-1):\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "line-200\n") {
		t.Fatalf("--scrollback -1 should include the last line (line-200):\n%s", buf.String())
	}
}

func countLines(s, prefix string) int {
	n := 0
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}
