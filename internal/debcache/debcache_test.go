package debcache

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// stubDocker replaces runDocker with handler for the duration of the test.
func stubDocker(t *testing.T, handler func(args []string) (string, error)) {
	t.Helper()
	orig := runDocker
	runDocker = func(args ...string) (string, error) { return handler(args) }
	t.Cleanup(func() { runDocker = orig })
}

// stubEnsureNetwork makes the network ensure a no-op returning a fixed name.
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
		"alpha":    "fleet-alpha-aptcache",
		"My Fleet": "fleet-my-fleet-aptcache",
		"a/b:c":    "fleet-a-b-c-aptcache",
	}
	for in, want := range cases {
		if got := ContainerName(in); got != want {
			t.Errorf("ContainerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProxyURL(t *testing.T) {
	if got, want := ProxyURL("alpha"), "http://fleet-alpha-aptcache:3142"; got != want {
		t.Errorf("ProxyURL = %q, want %q", got, want)
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
		image,
		state.DebCacheDir("alpha") + ":" + containerCachePath,
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
			return "", fmt.Errorf("No such object") // absent
		}
		return "", nil
	})

	dir, err := EnsureSharedServer("alpha")
	if err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if dir != state.DebCacheDir("alpha") {
		t.Fatalf("dir = %q, want %q", dir, state.DebCacheDir("alpha"))
	}
	if !hasCall(calls, "run") {
		t.Fatalf("expected a docker run, calls=%v", calls)
	}
	if _, err := os.Stat(state.DebCacheDir("alpha")); err != nil {
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
			return "false", nil // exists, stopped
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

func TestEnsureSharedServerReusesWhenRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubEnsureNetwork(t)
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "true", nil // running
		}
		return "", nil
	})
	if _, err := EnsureSharedServer("alpha"); err != nil {
		t.Fatalf("EnsureSharedServer: %v", err)
	}
	if hasCall(calls, "run") || hasCall(calls, "start") {
		t.Fatalf("running container should be reused: %v", calls)
	}
}

func TestSharedDirRejectsTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, bad := range []string{"", "..", "../../etc", "a/b", `a\b`, "foo/../bar"} {
		if _, err := SharedDir(bad); err == nil {
			t.Errorf("SharedDir(%q) should be rejected", bad)
		}
	}
	if dir, err := SharedDir("alpha"); err != nil || dir != state.DebCacheDir("alpha") {
		t.Errorf("SharedDir(alpha) = %q, %v; want %q, nil", dir, err, state.DebCacheDir("alpha"))
	}
}

func TestDeleteCacheWipesAndRestarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubEnsureNetwork(t)
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "inspect" {
			return "", fmt.Errorf("No such object") // absent → restart docker-runs
		}
		return "", nil
	})

	dir := state.DebCacheDir("alpha")
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
		t.Fatalf("expected docker rm (stop) + run (restart), calls=%v", calls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale cache blob not removed: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cache dir should still exist after wipe: %v", err)
	}
	if afterIno := inodeOf(t, dir); afterIno != beforeIno {
		t.Fatalf("cache dir inode changed (%d -> %d): would orphan bind mounts", beforeIno, afterIno)
	}
}

func TestStopSharedServer(t *testing.T) {
	stubDocker(t, func([]string) (string, error) { return "", nil })
	if err := StopSharedServer("alpha"); err != nil {
		t.Fatalf("StopSharedServer clean: %v", err)
	}
	stubDocker(t, func([]string) (string, error) {
		return "Error: No such container: fleet-alpha-aptcache", fmt.Errorf("exit status 1")
	})
	if err := StopSharedServer("alpha"); err != nil {
		t.Fatalf("StopSharedServer absent should be nil, got %v", err)
	}
	stubDocker(t, func([]string) (string, error) {
		return "permission denied", fmt.Errorf("exit status 1")
	})
	if err := StopSharedServer("alpha"); err == nil {
		t.Fatalf("StopSharedServer genuine failure should error")
	}
}

// TestEnsureDirToleratesForeignOwnedDir guards the re-create-after-destroy fix:
// apt-cacher-ng chowns the preserved .aptcache dir to its own uid, so on a later
// re-ensure a non-root host user can't chmod it (EPERM). ensureDir must treat
// that as benign for a PRE-EXISTING dir (it was made world-writable on first
// creation) but still surface a chmod failure on a freshly-created dir.
func TestEnsureDirToleratesForeignOwnedDir(t *testing.T) {
	orig := chmodDir
	chmodDir = func(string, os.FileMode) error { return os.ErrPermission }
	t.Cleanup(func() { chmodDir = orig })

	// Pre-existing dir + chmod EPERM → tolerated (nil).
	existing := filepath.Join(t.TempDir(), "aptcache")
	if err := os.MkdirAll(existing, 0o777); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ensureDir(existing); err != nil {
		t.Fatalf("ensureDir on a pre-existing foreign-owned dir must tolerate chmod EPERM, got %v", err)
	}

	// Freshly-created dir + chmod EPERM → real error (we should own a dir we just
	// made, so a failure here is genuine, not the foreign-owner case).
	fresh := filepath.Join(t.TempDir(), "nope", "aptcache")
	if err := ensureDir(fresh); err == nil {
		t.Fatalf("ensureDir must surface a chmod failure on a freshly-created dir")
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
	// The probe is the script that prints PRESENT/ABSENT; detect it by the
	// apt-get + apt.conf.d combination unique to probeScript.
	if strings.Contains(joined, "command -v apt-get") {
		out = f.probeOut
	}
	return backend.NewCmd(exec.Command("printf", "%s", out), nil)
}

func TestAptPresent(t *testing.T) {
	cases := map[string]bool{
		"PRESENT": true, "PRESENT\n": true, "warning\nPRESENT\n": true,
		"ABSENT": false, "": false,
	}
	for in, want := range cases {
		if got := aptPresent(in); got != want {
			t.Errorf("aptPresent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProbeAndConfigureScripts(t *testing.T) {
	probe := probeScript()
	for _, want := range []string{"command -v apt-get", "/etc/apt/apt.conf.d", probeMarkerPresent, probeMarkerAbsent} {
		if !strings.Contains(probe, want) {
			t.Errorf("probeScript missing %q: %s", want, probe)
		}
	}
	cfg := configureScript("http://fleet-alpha-aptcache:3142")
	for _, want := range []string{
		`Acquire::http::Proxy "http://fleet-alpha-aptcache:3142";`,
		proxyConfFile,
		"sudo tee",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configureScript missing %q: %s", want, cfg)
		}
	}
}

func TestConfigureInstanceAptSkipsWhenAbsent(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerAbsent}
	if err := ConfigureInstanceApt(fe, "/ws", ProxyURL("alpha")); err != nil {
		t.Fatalf("ConfigureInstanceApt (absent) = %v, want nil", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only the probe call, got %d: %v", len(fe.calls), fe.calls)
	}
}

func TestConfigureInstanceAptConfiguresWhenPresent(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerPresent}
	if err := ConfigureInstanceApt(fe, "/ws", ProxyURL("alpha")); err != nil {
		t.Fatalf("ConfigureInstanceApt (present) = %v, want nil", err)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("expected probe + configure, got %d: %v", len(fe.calls), fe.calls)
	}
	if !strings.Contains(fe.calls[1], proxyConfFile) {
		t.Fatalf("configure call did not write the proxy drop-in: %v", fe.calls[1])
	}
}
