//go:build unix

package mic

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "FLEET_MIC_TEST_HELPER_PROVIDER"

// TestHelperProvider is not a test: it is the body of the child process the
// tests below start, playing the part of `fleet` with the microphone live. It
// starts a capture, reports readiness on stdout, and then just waits to be
// killed — it NEVER calls Stop, because the point is a death with no cleanup.
func TestHelperProvider(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process only")
	}
	capture, err := Start("", func([]byte) {})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	_ = capture
	fmt.Println("recording")
	time.Sleep(time.Hour)
}

// startDoomedProvider runs TestHelperProvider in a child with a recorder stub of
// the hardest-to-close shape — a script file (so the recorder is the provider's
// GRANDCHILD, in its own process group) whose loop shrugs off EPIPE (so losing
// its reader does not stop it) — and returns the provider and the recorder pid.
func startDoomedProvider(t *testing.T) (provider *exec.Cmd, recorderPID int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "recorder.pid")
	script := filepath.Join(dir, "capture.sh")
	body := "#!/bin/sh\necho $$ > " + pidFile + "\nwhile :; do head -c 320 /dev/zero; sleep 0.02; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	provider = exec.Command(os.Args[0], "-test.run=^TestHelperProvider$")
	provider.Env = append(os.Environ(), helperEnv+"=1", EnvCapture+"="+script)
	stdout, err := provider.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Process.Kill(); _ = provider.Wait() })

	ready := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := stdout.Read(buf)
		ready <- string(buf[:n])
	}()
	select {
	case line := <-ready:
		if !strings.HasPrefix(line, "recording") {
			t.Fatalf("helper provider said %q", line)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("helper provider never started recording")
	}

	waitFor(t, "the recorder to write its pid", func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		recorderPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && recorderPID > 0
	})
	if err := syscall.Kill(recorderPID, 0); err != nil {
		t.Fatalf("setup: the recorder (pid %d) should be running: %v", recorderPID, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(recorderPID, syscall.SIGKILL) })
	return provider, recorderPID
}

// The microphone must close when fleet dies — HOWEVER it dies. Cancel-time
// cleanup cannot cover a death that runs no code: SIGKILL, or SIGHUP (closing
// the terminal; fleet does not handle it), or a panic. The recorder lives in its
// own process group, so the terminal's SIGHUP does not reach it either. What
// does: the lifeline — a dead process holds no pipes.
func TestRecorderDiesWithFleetHoweverFleetDies(t *testing.T) {
	// (Subtest names end up in t.TempDir(), and so in the FLEET_MIC_CAPTURE shell
	// command: keep them free of shell metacharacters.)
	for name, sig := range map[string]syscall.Signal{
		"SIGKILL-no-cleanup-possible": syscall.SIGKILL,
		"SIGHUP-terminal-closed":      syscall.SIGHUP,
		"SIGTERM-unhandled":           syscall.SIGTERM,
	} {
		t.Run(name, func(t *testing.T) {
			provider, recorder := startDoomedProvider(t)
			if err := provider.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_ = provider.Wait()
			waitFor(t, "the recorder to die with its provider", func() bool {
				return errors.Is(syscall.Kill(recorder, 0), syscall.ESRCH)
			})
		})
	}
}

// A recorder that exits on its own must not leave its watchdog behind: release
// closes the lifeline, and the watchdog goes with it.
func TestLifelineWatchdogDoesNotOutliveTheRecorder(t *testing.T) {
	cmd, release, err := guardedCommand(t.Context(), "sh", "-c", "echo done")
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	group := cmd.Process.Pid // Setpgid: the group id is the wrapper's pid
	release()
	waitFor(t, "the recorder's process group to be empty", func() bool {
		return errors.Is(syscall.Kill(-group, 0), syscall.ESRCH)
	})
}
