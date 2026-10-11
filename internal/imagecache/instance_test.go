package imagecache

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// scriptRig runs the probe and configure scripts for real under root: the
// paths they name (/etc/docker, the pod engine's folder, /proc) point into
// root, sudo runs its command as the test's user, kill only records what it
// was asked, and PATH holds only what the scripts use.
type scriptRig struct {
	t    *testing.T
	root string
	bin  string
	env  []string
}

func newScriptRig(t *testing.T, tools ...string) *scriptRig {
	t.Helper()
	root := t.TempDir()
	r := &scriptRig{t: t, root: root, bin: filepath.Join(root, "bin")}
	for _, d := range []string{r.bin, r.path("/etc"), r.path("/opt/fleet/docker"), r.path("/proc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range append([]string{"id", "mkdir", "tr", "cat", "tee", "mv"}, tools...) {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s here", tool)
		}
		if err := os.Symlink(p, filepath.Join(r.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	r.write("/bin/sudo", "#!/bin/sh\nexec \"$@\"\n", 0o755)
	r.write("/bin/kill", "#!/bin/sh\necho \"$*\" >> \""+r.path("/kills")+"\"\n", 0o755)
	r.env = []string{"PATH=" + r.bin, "HOME=" + root}
	return r
}

// path is p (an absolute in-instance path) under the rig's root.
func (r *scriptRig) path(p string) string { return filepath.Join(r.root, p) }

func (r *scriptRig) write(p, content string, mode os.FileMode) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.path(p)), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.path(p), []byte(content), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *scriptRig) read(p string) string {
	data, _ := os.ReadFile(r.path(p))
	return string(data)
}

// dockerd makes /proc show a dockerd with pid.
func (r *scriptRig) dockerd(pid string) { r.write("/proc/"+pid+"/comm", "dockerd\n", 0o644) }

// podEngine sets FLEET_DOCKERD_CONFIG to the engine's daemon.json under
// root, holding content.
func (r *scriptRig) podEngine(content string) {
	r.write(PodEngineConfig, content, 0o644)
	r.env = append(r.env, DockerdConfigEnv+"="+r.path(PodEngineConfig))
}

func (r *scriptRig) run(script string) (string, error) {
	r.t.Helper()
	for _, p := range []string{"/etc/docker", "/opt/fleet/docker", "/proc/[0-9]*"} {
		script = strings.ReplaceAll(script, p, r.path(p))
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *scriptRig) configure() {
	r.t.Helper()
	if out, err := r.run(configureScript("http://fs-alpha-1a2b3c4d-imgcache:5000", "fs-alpha-1a2b3c4d-imgcache:5000")); err != nil {
		r.t.Fatalf("configure: %v\n%s", err, out)
	}
}

// mirrorConfig is daemon.json with the mirror merged into keep.
func mirrorConfig(keep map[string]any) map[string]any {
	want := map[string]any{
		"registry-mirrors":    []any{"http://fs-alpha-1a2b3c4d-imgcache:5000"},
		"insecure-registries": []any{"fs-alpha-1a2b3c4d-imgcache:5000"},
	}
	for k, v := range keep {
		want[k] = v
	}
	return want
}

func sameJSON(t *testing.T, what, got string, want map[string]any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil || !reflect.DeepEqual(m, want) {
		t.Fatalf("%s = %s (%v), want %v", what, got, err, want)
	}
}

// TestProbeFindsThePodEngine: on a cluster a docker-outside-of-docker dev
// container sees no dockerd, yet its pod's engine (named by
// FLEET_DOCKERD_CONFIG, or found by its socket) is one fleet configures; one
// whose folder the user may not write and cannot sudo to is DENIED, so the
// poll stops at once. On a docker host such an instance has neither, and
// stays ABSENT; an instance's own dockerd is PRESENT as before.
func TestProbeFindsThePodEngine(t *testing.T) {
	t.Run("named by the env", func(t *testing.T) {
		r := newScriptRig(t)
		r.podEngine("{}\n")
		if out, err := r.run(probeScript()); err != nil || out != probeMarkerPresent {
			t.Fatalf("probe = %q, %v", out, err)
		}
	})
	t.Run("found by its socket", func(t *testing.T) {
		r := newScriptRig(t)
		l, err := net.Listen("unix", r.path(PodEngineSocket))
		if err != nil {
			t.Skipf("no unix socket at %s: %v", r.path(PodEngineSocket), err)
		}
		defer l.Close()
		if out, err := r.run(probeScript()); err != nil || out != probeMarkerPresent {
			t.Fatalf("probe = %q, %v", out, err)
		}
	})
	t.Run("root's folder and no sudo", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes any folder")
		}
		r := newScriptRig(t)
		r.podEngine("{}\n")
		if err := os.Remove(filepath.Join(r.bin, "sudo")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(r.path("/opt/fleet/docker"), 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(r.path("/opt/fleet/docker"), 0o755) })
		if out, err := r.run(probeScript()); err != nil || out != probeMarkerDenied {
			t.Fatalf("probe = %q, %v", out, err)
		}
	})
	t.Run("docker-outside-of-docker on a docker host", func(t *testing.T) {
		r := newScriptRig(t)
		r.write("/proc/7/comm", "sh\n", 0o644)
		if out, err := r.run(probeScript()); err != nil || out != probeMarkerAbsent {
			t.Fatalf("probe = %q, %v", out, err)
		}
	})
	t.Run("its own dockerd", func(t *testing.T) {
		r := newScriptRig(t)
		r.dockerd("4242")
		if out, err := r.run(probeScript()); err != nil || out != probeMarkerPresent {
			t.Fatalf("probe = %q, %v", out, err)
		}
	})
}

// TestConfigurePodEngine: the pod engine's daemon.json gets the mirror, its
// other keys kept, and no signal goes anywhere (the engine reloads its own
// file; a dockerd this container can see is not it). The empty object the
// engine starts with needs neither python3 nor jq; a file with keys and
// neither tool is left alone.
func TestConfigurePodEngine(t *testing.T) {
	for _, tt := range []struct {
		name, tool, content string
		want                map[string]any
	}{
		{"its first config, no tools", "", "{}\n", mirrorConfig(nil)},
		{"python3 keeps its keys", "python3", `{"mtu": 1450}`, mirrorConfig(map[string]any{"mtu": float64(1450)})},
		{"jq keeps its keys", "jq", `{"mtu": 1450}`, mirrorConfig(map[string]any{"mtu": float64(1450)})},
		{"again: once", "python3", `{"registry-mirrors": ["http://fs-alpha-1a2b3c4d-imgcache:5000"]}`, mirrorConfig(nil)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var tools []string
			if tt.tool != "" {
				tools = append(tools, tt.tool)
			}
			r := newScriptRig(t, tools...)
			r.podEngine(tt.content)
			r.dockerd("4242")
			r.configure()
			sameJSON(t, "the engine's daemon.json", r.read(PodEngineConfig), tt.want)
			if kills := r.read("/kills"); kills != "" {
				t.Fatalf("signalled %q: the pod's engine reloads by itself", kills)
			}
			if left, _ := filepath.Glob(r.path(PodEngineConfig) + ".*"); len(left) != 0 {
				t.Fatalf("temp files left: %v", left)
			}
		})
	}
	t.Run("keys and no tools: left alone", func(t *testing.T) {
		r := newScriptRig(t)
		r.podEngine(`{"mtu": 1450}`)
		r.configure()
		if got := r.read(PodEngineConfig); got != `{"mtu": 1450}` {
			t.Fatalf("daemon.json = %q, want it untouched", got)
		}
	})
	t.Run("found by its socket", func(t *testing.T) {
		r := newScriptRig(t)
		l, err := net.Listen("unix", r.path(PodEngineSocket))
		if err != nil {
			t.Skipf("no unix socket at %s: %v", r.path(PodEngineSocket), err)
		}
		defer l.Close()
		r.write(PodEngineConfig, "{}\n", 0o644)
		r.dockerd("4242")
		r.configure()
		sameJSON(t, "the engine's daemon.json", r.read(PodEngineConfig), mirrorConfig(nil))
		if _, err := os.Stat(r.path("/etc/docker/daemon.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("/etc/docker/daemon.json written beside a pod engine: %v", err)
		}
		if kills := r.read("/kills"); kills != "" {
			t.Fatalf("signalled %q", kills)
		}
	})
}

// TestConfigureInstanceDockerd: the instance's own dockerd reads
// /etc/docker/daemon.json and is sent SIGHUP to reload it, as before.
func TestConfigureInstanceDockerd(t *testing.T) {
	r := newScriptRig(t, "python3")
	r.write("/etc/docker/daemon.json", `{"mtu": 1450}`, 0o644)
	r.dockerd("4242")
	r.configure()
	sameJSON(t, "/etc/docker/daemon.json", r.read("/etc/docker/daemon.json"), mirrorConfig(map[string]any{"mtu": float64(1450)}))
	if kills := strings.TrimSpace(r.read("/kills")); kills != "-HUP 4242" {
		t.Fatalf("kills %q, want dockerd 4242 sent SIGHUP", kills)
	}
}

// A pod engine fleet may not write is a warning at once, never configured
// and not polled for.
func TestConfigureInstanceDockerDeniedPodEngine(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerDenied}
	err := ConfigureInstanceDocker(fe, "/ws", MirrorURL("alpha"), MirrorHostPort("alpha"))
	if !errors.Is(err, errPodEngineDenied) || len(fe.calls) != 1 {
		t.Fatalf("ConfigureInstanceDocker = %v after %d calls, want the denial after the probe alone", err, len(fe.calls))
	}
	setPollTimings(t, time.Hour, time.Hour, 3)
	done := make(chan error, 1)
	ConfigureInstanceDockerPolling(&fakeExecer{probeOut: probeMarkerDenied}, "/ws", MirrorURL("alpha"), MirrorHostPort("alpha"),
		func(configured bool, err error) {
			if !configured {
				err = errors.New("reported not configured")
			}
			done <- err
		})
	select {
	case err := <-done:
		if !errors.Is(err, errPodEngineDenied) {
			t.Fatalf("poll result %v, want the denial", err)
		}
	default:
		t.Fatal("the poll went on past a denial (its next probe would be an hour away)")
	}
}
