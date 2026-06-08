package imagecache

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
