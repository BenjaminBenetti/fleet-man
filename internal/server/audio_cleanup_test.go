package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func audioCleanupSeams(t *testing.T) {
	t.Helper()
	stop, settings, timeout, delay := stopMicServer, micSetting, micStopTimeout, micStopRetryDelay
	t.Cleanup(func() {
		stopMicServer, micSetting, micStopTimeout, micStopRetryDelay = stop, settings, timeout, delay
	})
	micSetting = func() (state.MicSettings, error) { return state.MicSettings{}, nil }
	micStopRetryDelay = time.Millisecond
}

func TestMicCleanupRetriesAndBoundsFailures(t *testing.T) {
	for _, failures := range []int{0, 2, 5} {
		t.Run(fmt.Sprintf("failures=%d", failures), func(t *testing.T) {
			audioCleanupSeams(t)
			calls := 0
			stopMicServer = func(*fleet.Instance) error {
				calls++
				if calls <= failures {
					return errors.New("fallback control failed")
				}
				return nil
			}
			stopMicWhenDisabled(&fleet.Instance{Name: "test"})
			if want := min(failures+1, 3); calls != want {
				t.Fatalf("stop calls = %d, want %d", calls, want)
			}
		})
	}
}

func TestMicCleanupRechecksSettings(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "re-enabled", true: "unreadable"}[unreadable], func(t *testing.T) {
			audioCleanupSeams(t)
			calls := 0
			var enabled atomic.Bool
			micSetting = func() (state.MicSettings, error) {
				if unreadable && enabled.Load() {
					return state.MicSettings{}, errors.New("config unavailable")
				}
				return state.MicSettings{Enabled: enabled.Load()}, nil
			}
			stopMicServer = func(*fleet.Instance) error {
				calls++
				enabled.Store(true)
				return errors.New("fallback control failed")
			}
			stopMicWhenDisabled(&fleet.Instance{})
			if calls != 1 {
				t.Fatalf("stale cleanup ran %d times after settings changed", calls)
			}
		})
	}
}

func TestMicCleanupBoundsBackendWait(t *testing.T) {
	audioCleanupSeams(t)
	micStopTimeout = 5 * time.Millisecond
	var calls atomic.Int32
	var workers sync.WaitGroup
	workers.Add(3)
	release := make(chan struct{})
	t.Cleanup(func() { close(release); workers.Wait() })
	stopMicServer = func(*fleet.Instance) error {
		defer workers.Done()
		calls.Add(1)
		<-release
		return nil
	}
	started := time.Now()
	stopMicWhenDisabled(&fleet.Instance{})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cleanup blocked for %v", elapsed)
	}
	if calls.Load() != 3 {
		t.Fatalf("backend calls = %d, want 3", calls.Load())
	}
}

// This runtime is mocked; it verifies the daemon's actual command sequence and
// error propagation. The in-instance routing and lease tests use real PulseAudio.
func audioCleanupRuntime(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "commands")
	t.Setenv("FLEET_TEST_AUDIO_COMMANDS", log)
	script := `#!/bin/sh
if [ "$1" = inspect ]; then echo vscode; exit 0; fi
case "$*" in
  *' mic stop'*|*' output stop'*) ;;
  *) echo vscode; exit 0;;
esac
printf '%s\n' "$*" >> "$FLEET_TEST_AUDIO_COMMANDS"
case "$*" in
  *'output stop'*)
    if [ "$FLEET_TEST_OUTPUT_STOP_FAIL" = 1 ]; then exit 1; fi;;
  *'mic stop'*)
    if [ "$FLEET_TEST_MIC_STOP_FAIL" = 1 ]; then
      echo 'leaving microphone loaded: injected control failure' >&2
      exit 1
    fi;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestOutputCleanupAlsoStopsDisabledMic(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		enabled, outputFailure bool
	}{
		{name: "both-off"},
		{name: "mic-re-enabled", enabled: true},
		{name: "output-stop-failed", outputFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioCleanupSeams(t)
			log := audioCleanupRuntime(t)
			if tc.outputFailure {
				t.Setenv("FLEET_TEST_OUTPUT_STOP_FAIL", "1")
			}
			micSetting = func() (state.MicSettings, error) { return state.MicSettings{Enabled: tc.enabled}, nil }
			stopOutputServer(&fleet.Instance{Name: "test", ContainerID: "audio-cleanup-test"})
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			commands := strings.Split(strings.TrimSpace(string(data)), "\n")
			want := 2
			if tc.enabled {
				want = 1
			}
			if len(commands) != want || !strings.Contains(commands[0], " output stop") || (!tc.enabled && !strings.Contains(commands[1], " mic stop")) {
				t.Fatalf("unexpected cleanup sequence: %q", commands)
			}
		})
	}
}

func TestMicCleanupWarningsIncludeRemoteFailure(t *testing.T) {
	if os.Getenv("FLEET_TEST_MIC_CLEANUP_LOG") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMicCleanupWarningsIncludeRemoteFailure$")
		cmd.Env = append(os.Environ(), "FLEET_TEST_MIC_CLEANUP_LOG=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("log subprocess: %v\n%s", err, out)
		}
		return
	}
	// Isolate the process-global logger as well as the fake runtime.
	isolateFleetDir(t)
	audioCleanupSeams(t)
	audioCleanupRuntime(t)
	t.Setenv("FLEET_TEST_MIC_STOP_FAIL", "1")
	stopMicWhenDisabled(&fleet.Instance{Name: "test", ContainerID: "audio-cleanup-test"})
	data, err := os.ReadFile(flog.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level=WARN", "injected control failure", "attempt=3", "exhausted retries"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %q in log:\n%s", want, data)
		}
	}
}
