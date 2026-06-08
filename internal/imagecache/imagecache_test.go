package imagecache

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func stubDocker(t *testing.T, handler func(args []string) (string, error)) {
	t.Helper()
	orig := runDocker
	runDocker = func(args ...string) (string, error) { return handler(args) }
	t.Cleanup(func() { runDocker = orig })
}

func stubEnsureNetwork(t *testing.T) {
	t.Helper()
	orig := ensureNetwork
	ensureNetwork = func(fleet string) (string, error) { return "fleet-" + fleet + "-net", nil }
	t.Cleanup(func() { ensureNetwork = orig })
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
		"alpha":    "fleet-alpha-imgcache",
		"My Fleet": "fleet-my-fleet-imgcache",
	}
	for in, want := range cases {
		if got := ContainerName(in); got != want {
			t.Errorf("ContainerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMirrorAddresses(t *testing.T) {
	if got, want := MirrorURL("alpha"), "http://fleet-alpha-imgcache:5000"; got != want {
		t.Errorf("MirrorURL = %q, want %q", got, want)
	}
	if got, want := MirrorHostPort("alpha"), "fleet-alpha-imgcache:5000"; got != want {
		t.Errorf("MirrorHostPort = %q, want %q", got, want)
	}
}

func TestDockerRunArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := dockerRunArgs("alpha", "fleet-alpha-net")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run", "-d", "--restart", "unless-stopped",
		ContainerName("alpha"),
		"--network fleet-alpha-net",
		"REGISTRY_PROXY_REMOTEURL=" + upstreamRegistry,
		image,
		state.ImageCacheDir("alpha") + ":" + containerCachePath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dockerRunArgs missing %q in: %s", want, joined)
		}
	}
}

func TestEnsureSharedServerCreatesWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubEnsureNetwork(t)
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "", fmt.Errorf("No such object")
		}
		return "", nil
	})
	dir, err := EnsureSharedServer("alpha")
	if err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if dir != state.ImageCacheDir("alpha") {
		t.Fatalf("dir = %q, want %q", dir, state.ImageCacheDir("alpha"))
	}
	if !hasCall(calls, "run") {
		t.Fatalf("expected a docker run, calls=%v", calls)
	}
	if _, err := os.Stat(state.ImageCacheDir("alpha")); err != nil {
		t.Fatalf("cache dir not created: %v", err)
	}
}

func TestEnsureSharedServerStartsWhenStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubEnsureNetwork(t)
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "false", nil
		}
		return "", nil
	})
	if _, err := EnsureSharedServer("alpha"); err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if !hasCall(calls, "start") || hasCall(calls, "run") {
		t.Fatalf("stopped container should be started, not run: %v", calls)
	}
}

func TestSharedDirRejectsTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, bad := range []string{"", "..", "../../etc", "a/b"} {
		if _, err := SharedDir(bad); err == nil {
			t.Errorf("SharedDir(%q) should be rejected", bad)
		}
	}
	if dir, err := SharedDir("alpha"); err != nil || dir != state.ImageCacheDir("alpha") {
		t.Errorf("SharedDir(alpha) = %q, %v; want %q, nil", dir, err, state.ImageCacheDir("alpha"))
	}
}

func TestDeleteCacheWipesAndRestarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubEnsureNetwork(t)
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "", fmt.Errorf("No such object")
		}
		return "", nil
	})

	dir := state.ImageCacheDir("alpha")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("seed cache dir: %v", err)
	}
	marker := filepath.Join(dir, "old-blob")
	if err := os.WriteFile(marker, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	beforeIno := inodeOf(t, dir)

	if err := DeleteCache("alpha"); err != nil {
		t.Fatalf("DeleteCache: %v", err)
	}
	if !hasCall(calls, "rm") || !hasCall(calls, "run") {
		t.Fatalf("expected docker rm + run, calls=%v", calls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale blob not removed: %v", err)
	}
	if afterIno := inodeOf(t, dir); afterIno != beforeIno {
		t.Fatalf("cache dir inode changed: would orphan bind mounts")
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("inode check unsupported on this platform")
	}
	return st.Ino
}

// --- instance config ---

type fakeExecer struct {
	probeOut string
	calls    []string
}

func (f *fakeExecer) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	joined := strings.Join(command, " ")
	f.calls = append(f.calls, joined)
	out := ""
	// The probe script is the one that prints the PRESENT/ABSENT markers; the
	// configure script never does, so this distinguishes the two.
	if strings.Contains(joined, probeMarkerPresent) {
		out = f.probeOut
	}
	return backend.NewCmd(exec.Command("printf", "%s", out), nil)
}

func TestDockerdPresent(t *testing.T) {
	cases := map[string]bool{"PRESENT": true, "warning\nPRESENT\n": true, "ABSENT": false, "": false}
	for in, want := range cases {
		if got := dockerdPresent(in); got != want {
			t.Errorf("dockerdPresent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProbeAndConfigureScripts(t *testing.T) {
	probe := probeScript()
	for _, want := range []string{"/proc/[0-9]*/comm", `[ "$n" = dockerd ]`, "/etc/docker", probeMarkerPresent, probeMarkerAbsent} {
		if !strings.Contains(probe, want) {
			t.Errorf("probeScript missing %q: %s", want, probe)
		}
	}
	cfg := configureScript(MirrorURL("alpha"), MirrorHostPort("alpha"))
	for _, want := range []string{
		"MIRROR='http://fleet-alpha-imgcache:5000'",
		"INSECURE='fleet-alpha-imgcache:5000'",
		"/etc/docker/daemon.json",
		"registry-mirrors",
		"insecure-registries",
		"kill -HUP",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configureScript missing %q: %s", want, cfg)
		}
	}
}

func TestConfigureInstanceDockerSkipsWhenAbsent(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerAbsent}
	if err := ConfigureInstanceDocker(fe, "/ws", MirrorURL("alpha"), MirrorHostPort("alpha")); err != nil {
		t.Fatalf("ConfigureInstanceDocker (absent) = %v, want nil", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only the probe call, got %d: %v", len(fe.calls), fe.calls)
	}
}

func TestConfigureInstanceDockerConfiguresWhenPresent(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerPresent}
	if err := ConfigureInstanceDocker(fe, "/ws", MirrorURL("alpha"), MirrorHostPort("alpha")); err != nil {
		t.Fatalf("ConfigureInstanceDocker (present) = %v, want nil", err)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("expected probe + configure, got %d: %v", len(fe.calls), fe.calls)
	}
	if !strings.Contains(fe.calls[1], "registry-mirrors") {
		t.Fatalf("configure call did not write the mirror config: %v", fe.calls[1])
	}
}

// pollExecer is a concurrency-safe fake (the background phase calls ExecCommand
// from a goroutine) whose probe reports ABSENT until presentAtProbe probes have
// run, then PRESENT. presentAtProbe == 0 means dockerd never appears.
type pollExecer struct {
	mu             sync.Mutex
	presentAtProbe int
	probeCalls     int
	configCalls    int
}

func (f *pollExecer) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	joined := strings.Join(command, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	out := ""
	// Only the probe script prints the markers; the configure script does not.
	if strings.Contains(joined, probeMarkerPresent) {
		f.probeCalls++
		if f.presentAtProbe != 0 && f.probeCalls >= f.presentAtProbe {
			out = probeMarkerPresent
		} else {
			out = probeMarkerAbsent
		}
	} else {
		f.configCalls++
	}
	return backend.NewCmd(exec.Command("printf", "%s", out), nil)
}

func (f *pollExecer) counts() (probes, configs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeCalls, f.configCalls
}

// setPollTimings shrinks the polling schedule for fast tests and restores it.
func setPollTimings(t *testing.T, interval, timeout time.Duration, sync int) {
	t.Helper()
	oi, ot, os := pollInterval, pollTimeout, pollSyncAttempts
	pollInterval, pollTimeout, pollSyncAttempts = interval, timeout, sync
	t.Cleanup(func() { pollInterval, pollTimeout, pollSyncAttempts = oi, ot, os })
}

// awaitResult runs ConfigureInstanceDockerPolling and returns its single
// onResult outcome, failing if the callback never fires.
func awaitResult(t *testing.T, fe *pollExecer) (configured bool, err error) {
	t.Helper()
	type res struct {
		configured bool
		err        error
	}
	ch := make(chan res, 1)
	ConfigureInstanceDockerPolling(fe, "/ws", MirrorURL("alpha"), MirrorHostPort("alpha"),
		func(configured bool, err error) { ch <- res{configured, err} })
	select {
	case r := <-ch:
		return r.configured, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("onResult never fired")
		return false, nil
	}
}

// TestConfigureInstanceDockerPollingSyncHit verifies a dockerd present on the
// first probe is configured synchronously (probe + configure, no extra probes).
func TestConfigureInstanceDockerPollingSyncHit(t *testing.T) {
	setPollTimings(t, time.Millisecond, time.Second, 3)
	fe := &pollExecer{presentAtProbe: 1}
	configured, err := awaitResult(t, fe)
	if !configured || err != nil {
		t.Fatalf("sync hit = (%v, %v), want (true, nil)", configured, err)
	}
	if probes, configs := fe.counts(); probes != 1 || configs != 1 {
		t.Fatalf("expected exactly 1 probe + 1 configure, got probes=%d configs=%d", probes, configs)
	}
}

// TestConfigureInstanceDockerPollingBackgroundHit verifies that when dockerd does
// not appear during the synchronous attempts, the background rescue loop keeps
// probing and configures once it shows up.
func TestConfigureInstanceDockerPollingBackgroundHit(t *testing.T) {
	setPollTimings(t, time.Millisecond, 2*time.Second, 1)
	fe := &pollExecer{presentAtProbe: 3} // absent for the sync probe, then appears
	configured, err := awaitResult(t, fe)
	if !configured || err != nil {
		t.Fatalf("background hit = (%v, %v), want (true, nil)", configured, err)
	}
	if probes, configs := fe.counts(); probes < 3 || configs != 1 {
		t.Fatalf("expected >=3 probes + 1 configure, got probes=%d configs=%d", probes, configs)
	}
}

// TestConfigureInstanceDockerPollingGivesUp verifies that if no dockerd ever
// appears the poll gives up after the timeout, reporting configured=false with no
// error (the documented "no dockerd, do nothing" outcome) and never configuring.
func TestConfigureInstanceDockerPollingGivesUp(t *testing.T) {
	setPollTimings(t, time.Millisecond, 30*time.Millisecond, 1)
	fe := &pollExecer{presentAtProbe: 0} // never present
	configured, err := awaitResult(t, fe)
	if configured || err != nil {
		t.Fatalf("give up = (%v, %v), want (false, nil)", configured, err)
	}
	if _, configs := fe.counts(); configs != 0 {
		t.Fatalf("expected no configure calls on give-up, got %d", configs)
	}
}
