//go:build !nok8s

package podspec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// sidecarOf is the instance pod's docker engine sidecar, nil without one.
func sidecarOf(o *Objects) *corev1.Container {
	for i, c := range o.StatefulSet.Spec.Template.Spec.InitContainers {
		if c.Name == DockerSidecar {
			return &o.StatefulSet.Spec.Template.Spec.InitContainers[i]
		}
	}
	return nil
}

// A run that binds the docker engine's socket gets the engine as a native
// sidecar: started first, kept running beside the dev container, which
// waits for its startup probe; privileged; its data on vol/fleet-docker; its
// socket and daemon.json in a folder the dev container shares; the
// workspace at the dev container's path and at the daemon host's.
func TestRenderDockerSidecar(t *testing.T) {
	o := mustRender(t, []string{"--mount", dockerSockBind}, func(in *Input, _ *RenderInput) {
		in.Cluster.DinDAddressPool = "base=10.200.0.0/16,size=26 10.201.0.0/16"
	})
	sc := sidecarOf(o)
	if sc == nil {
		t.Fatal("no docker sidecar")
	}
	if sc.RestartPolicy == nil || *sc.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("restart policy %v: not a native sidecar", sc.RestartPolicy)
	}
	if sc.Image != DefaultDockerImage || !strings.Contains(sc.Image, "@sha256:") {
		t.Errorf("image %s", sc.Image)
	}
	if sc.SecurityContext == nil || sc.SecurityContext.Privileged == nil || !*sc.SecurityContext.Privileged || *sc.SecurityContext.RunAsUser != 0 {
		t.Errorf("security %+v", sc.SecurityContext)
	}
	if got, want := strings.Join(sc.Command[len(DockerSidecarCommand()):], " "),
		"--default-address-pool base=10.200.0.0/16,size=26 --default-address-pool base=10.201.0.0/16,size=24"; got != want {
		t.Errorf("dockerd options %q, want %q", got, want)
	}
	if p := sc.StartupProbe; p == nil || p.Exec == nil || strings.Join(p.Exec.Command, " ") != strings.Join(dockerReadyCommand, " ") || p.FailureThreshold*p.PeriodSeconds < 60 {
		t.Errorf("startup probe %+v", p)
	}
	for at, want := range map[string]corev1.VolumeMount{
		DockerDir:                       {Name: volumeDocker, MountPath: DockerDir},
		"/var/lib/docker":               {Name: volumeDisk, MountPath: "/var/lib/docker", SubPath: podseed.VolumeSubPath(DockerDataVolume)},
		"/workspaces/app":               {Name: volumeDisk, MountPath: "/workspaces/app", SubPath: podseed.DirWorkspace},
		strings.TrimSuffix(baseWS, "/"): {Name: volumeDisk, MountPath: baseWS, SubPath: podseed.DirWorkspace},
	} {
		if got, ok := mountAt(*sc, at); !ok || got != want {
			t.Errorf("sidecar mount at %s: %+v, want %+v", at, got, want)
		}
	}
	if len(sc.Env) > 0 || len(sc.EnvFrom) > 0 {
		t.Errorf("the sidecar gets the run's environment: %+v", sc.Env)
	}

	dev := devOf(o)
	if m, ok := mountAt(dev, DockerDir); !ok || m.Name != volumeDocker {
		t.Errorf("the dev container does not share the sidecar's folder: %+v", dev.VolumeMounts)
	}
	if _, ok := mountAt(dev, "/var/run/docker-host.sock"); ok {
		t.Error("the socket target is mounted; pod-init links it instead")
	}
	var env *corev1.EnvVar
	for i, e := range dev.Env {
		if e.Name == DockerConfigEnv {
			env = &dev.Env[i]
		}
	}
	if env == nil || env.Value != DockerConfig {
		t.Errorf("the dev container's %s: %+v", DockerConfigEnv, env)
	}
	if dev.SecurityContext.Privileged != nil {
		t.Error("the dev container is privileged for the sidecar's sake")
	}
	var shared bool
	for _, v := range o.StatefulSet.Spec.Template.Spec.Volumes {
		if v.Name == volumeDocker && v.EmptyDir != nil && v.EmptyDir.Medium == "" {
			shared = true
		}
	}
	if !shared {
		t.Error("no shared emptyDir for the engine's socket")
	}

	// The connection's image; sysbox runs it unprivileged; the hash covers
	// the sidecar's privilege.
	other := mustRender(t, []string{"--mount", dockerSockBind}, func(in *Input, _ *RenderInput) {
		in.Cluster.ServiceImages.DinD = "registry.example.com/mirror/docker:29-dind"
		in.Facts.RuntimeClasses = []string{"sysbox-runc"}
	})
	sc = sidecarOf(other)
	if sc.Image != "registry.example.com/mirror/docker:29-dind" || sc.SecurityContext.Privileged != nil {
		t.Errorf("sysbox sidecar %s %+v", sc.Image, sc.SecurityContext)
	}
	if rc := other.StatefulSet.Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != "sysbox-runc" {
		t.Errorf("runtime class %v", rc)
	}
	if SpecHash(&other.StatefulSet.Spec.Template.Spec) == SpecHash(&o.StatefulSet.Spec.Template.Spec) {
		t.Error("the spec hash does not see the sidecar's privilege")
	}
	if sidecarOf(mustRender(t, []string{"--privileged"}, nil)) != nil {
		t.Error("a sidecar without a docker socket bind")
	}
}

// A pod without root (capability tier C) cannot have the engine, which runs
// as root.
func TestRenderDockerSidecarNeedsRoot(t *testing.T) {
	uid := int64(1000)
	_, err := renderOf(t, []string{"--mount", dockerSockBind}, func(_ *Input, r *RenderInput) {
		r.Identity = Identity{NonRoot: true, UID: &uid, GID: &uid}
	})
	if rs := Refusals(err); len(rs) != 1 || rs[0].Code != CodeDooDNeedsPrivileged {
		t.Fatalf("Render = %v, want %s", err, CodeDooDNeedsPrivileged)
	}
}

// Only workspace paths outside the sidecar's own dirs are mounted at the
// daemon host's path.
func TestSidecarMayMount(t *testing.T) {
	for p, want := range map[string]bool{
		"/home/dev/.fleet/workspaces/f/i": true, "/Users/dev/src/app": true, "/tmp/itest/ws": true,
		"/": false, "/usr/src/app": false, "/var/lib/docker/x": false, "/opt/fleet/x": false, "/proc/1": false,
		"relative": false, `C:\src`: false, "/run/x": false, "/etc": false,
	} {
		if got := sidecarMayMount(p); got != want {
			t.Errorf("sidecarMayMount(%q) = %v, want %v", p, got, want)
		}
	}
	// A workspace whose host path is the dev container's path is mounted
	// once.
	o := mustRender(t, []string{"--mount", dockerSockBind}, func(in *Input, _ *RenderInput) {
		in.WorkspaceFolder = baseWS
		in.Capture.Args = replaceArg(in.Capture.Args, "target=/workspaces/app", "target="+baseWS)
	})
	n := 0
	for _, m := range sidecarOf(o).VolumeMounts {
		if m.SubPath == podseed.DirWorkspace {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the workspace is mounted %d times in the sidecar", n)
	}
}

func replaceArg(args []string, old, new string) []string {
	out := append([]string(nil), args...)
	for i, a := range out {
		out[i] = strings.ReplaceAll(a, old, new)
	}
	return out
}

// The kubelet expands $(VAR) and turns $$ into $ in a container's command;
// fleet's sidecar script and probe have neither, so they run as written.
func TestDockerSidecarScriptPassesKubeletExpansion(t *testing.T) {
	for _, s := range append(DockerSidecarCommand(), dockerReadyCommand...) {
		if strings.Contains(s, "$(") || strings.Contains(s, "$$") {
			t.Errorf("%q would be expanded by the kubelet", s)
		}
	}
}

// The sidecar's script in a real shell, with a stand-in dockerd: it runs the
// engine on the shared socket and config with the pod network's MTU and the
// connection's options, sends it SIGHUP when the config changes (and not
// otherwise), passes SIGTERM on, and ends with the engine's status.
func TestDockerSidecarScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sidecar's script runs in a linux pod")
	}
	if os.Geteuid() == 0 {
		// It would remount / and write the cgroupfs of the machine.
		t.Skip("not as root")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{out, bin, filepath.Join(dir, "shared")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dockerd := `#!/bin/sh
echo "$@" > "$OUT/args"
trap 'echo HUP >> "$OUT/signals"' HUP
trap 'echo TERM >> "$OUT/signals"; exit 7' TERM
while :; do sleep 0.05; done
`
	if err := os.WriteFile(filepath.Join(bin, "dockerd"), []byte(dockerd), 0o755); err != nil {
		t.Fatal(err)
	}
	script := dockerSidecarScript
	shared := filepath.Join(dir, "shared")
	for old, new := range map[string]string{
		"dir=" + DockerDir + "\n":                  "dir=" + shared + "\n",
		"find /run /var/run ":                      "find " + shared + " ",
		"mount -t securityfs none ":                ": ",
		"mount --make-rshared / ":                  ": ",
		"! mountpoint -q /sys/kernel/security":     "false",
		"/sys/fs/cgroup${cg%/}":                    dir + "/no-cgroupfs",
		"[ -x /usr/local/sbin/.iptables-legacy/ip": "[ -x /nonexistent/ip",
	} {
		if !strings.Contains(script, old) {
			t.Fatalf("the script has no %q to point at the test's dirs", old)
		}
		script = strings.ReplaceAll(script, old, new)
	}
	cmd := exec.Command("/bin/sh", "-c", script, DockerSidecar, "--default-address-pool", "base=10.200.0.0/16,size=24")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "OUT="+out)
	logs := &syncLog{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened\n%s", what, logs)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(out, name))
		return string(data)
	}
	waitFor("dockerd started", func() bool { return read("args") != "" })
	args := read("args")
	for _, want := range []string{"--host=unix://" + shared + "/docker.sock", "--config-file=" + shared + "/daemon.json", "--mtu=", "--default-address-pool base=10.200.0.0/16,size=24"} {
		if !strings.Contains(args, want) {
			t.Errorf("dockerd %q lacks %q", args, want)
		}
	}
	if strings.Contains(args, "--cgroup-parent") {
		t.Errorf("dockerd %q: a cgroup parent without a writable cgroupfs", args)
	}
	if data, _ := os.ReadFile(filepath.Join(shared, "daemon.json")); strings.TrimSpace(string(data)) != "{}" {
		t.Errorf("daemon.json %q, want an empty config made for the engine", data)
	}
	time.Sleep(2500 * time.Millisecond)
	if s := read("signals"); s != "" {
		t.Fatalf("signals %q before the config changed", s)
	}
	if err := os.WriteFile(filepath.Join(shared, "daemon.json"), []byte(`{"registry-mirrors":["http://mirror:5000"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("a reload", func() bool { return read("signals") == "HUP\n" })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !strings.Contains(read("signals"), "TERM") || !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("after SIGTERM: %v, signals %q\n%s", err, read("signals"), logs)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the script did not end on SIGTERM\n%s", logs)
	}
}

// syncLog collects a process's output, written from two goroutines.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
