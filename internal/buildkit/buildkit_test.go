package buildkit

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// stubDocker replaces runDocker with handler for the duration of the test,
// recording nothing itself — the handler closes over whatever it needs.
func stubDocker(t *testing.T, handler func(args []string) (string, error)) {
	t.Helper()
	orig := runDocker
	runDocker = func(args ...string) (string, error) { return handler(args) }
	t.Cleanup(func() { runDocker = orig })
}

// stubWaitForSocket makes the socket wait return err immediately.
func stubWaitForSocket(t *testing.T, err error) {
	t.Helper()
	orig := waitForSocket
	waitForSocket = func(string, time.Duration) error { return err }
	t.Cleanup(func() { waitForSocket = orig })
}

func hasCall(calls [][]string, sub string) bool {
	for _, c := range calls {
		if len(c) > 0 && c[0] == sub {
			return true
		}
	}
	return false
}

func TestContainerName(t *testing.T) {
	cases := map[string]string{
		"alpha":     "fleet-alpha-buildkit",
		"My Fleet":  "fleet-my-fleet-buildkit",
		"a/b:c":     "fleet-a-b-c-buildkit",
		"UPPER":     "fleet-upper-buildkit",
		"--weird--": "fleet-weird-buildkit",
	}
	for in, want := range cases {
		if got := ContainerName(in); got != want {
			t.Errorf("ContainerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstanceMount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := InstanceMount("alpha")
	if m.LocalPath != state.BuildkitDir("alpha") {
		t.Errorf("LocalPath = %q, want %q", m.LocalPath, state.BuildkitDir("alpha"))
	}
	if m.ContainerPath != containerMountPath {
		t.Errorf("ContainerPath = %q, want %q", m.ContainerPath, containerMountPath)
	}
}

func TestDockerRunArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := dockerRunArgs("alpha")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run", "-d", "--privileged", "--restart", "unless-stopped",
		ContainerName("alpha"),
		image,
		"--addr", "unix://" + containerSocketPath,
		state.BuildkitDir("alpha") + ":" + containerMountPath,
		hostCacheDir("alpha") + ":" + containerCachePath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dockerRunArgs missing %q in: %s", want, joined)
		}
	}
}

func TestEnsureSharedServerCreatesWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "", fmt.Errorf("Error: No such object: x") // absent
		}
		return "", nil
	})
	stubWaitForSocket(t, nil)

	dir, err := EnsureSharedServer("alpha")
	if err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if dir != state.BuildkitDir("alpha") {
		t.Fatalf("dir = %q, want %q", dir, state.BuildkitDir("alpha"))
	}
	if !hasCall(calls, "run") {
		t.Fatalf("expected a docker run, calls = %v", calls)
	}
	if hasCall(calls, "start") {
		t.Fatalf("did not expect docker start for an absent container, calls = %v", calls)
	}
	// Host dirs must exist for the bind mount to succeed.
	if _, err := os.Stat(state.BuildkitDir("alpha")); err != nil {
		t.Fatalf("buildkit dir not created: %v", err)
	}
	if _, err := os.Stat(hostCacheDir("alpha")); err != nil {
		t.Fatalf("cache dir not created: %v", err)
	}
}

func TestEnsureSharedServerStartsWhenStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "false", nil // exists, stopped
		}
		return "", nil
	})
	stubWaitForSocket(t, nil)

	if _, err := EnsureSharedServer("alpha"); err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if !hasCall(calls, "start") {
		t.Fatalf("expected a docker start, calls = %v", calls)
	}
	if hasCall(calls, "run") {
		t.Fatalf("did not expect docker run for an existing stopped container, calls = %v", calls)
	}
}

func TestEnsureSharedServerReusesWhenRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "true", nil // exists, running
		}
		return "", nil
	})
	stubWaitForSocket(t, nil)

	if _, err := EnsureSharedServer("alpha"); err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if hasCall(calls, "run") || hasCall(calls, "start") {
		t.Fatalf("running container should be reused, not run/started: %v", calls)
	}
}

func TestStopSharedServer(t *testing.T) {
	// Clean removal -> nil.
	stubDocker(t, func(args []string) (string, error) { return "", nil })
	if err := StopSharedServer("alpha"); err != nil {
		t.Fatalf("StopSharedServer clean: %v", err)
	}

	// Already absent -> nil (idempotent teardown).
	stubDocker(t, func(args []string) (string, error) {
		return "Error: No such container: fleet-alpha-buildkit", fmt.Errorf("exit status 1")
	})
	if err := StopSharedServer("alpha"); err != nil {
		t.Fatalf("StopSharedServer absent should be nil, got %v", err)
	}

	// Genuine failure -> error.
	stubDocker(t, func(args []string) (string, error) {
		return "permission denied", fmt.Errorf("exit status 1")
	})
	if err := StopSharedServer("alpha"); err == nil {
		t.Fatalf("StopSharedServer genuine failure should error")
	}
}
