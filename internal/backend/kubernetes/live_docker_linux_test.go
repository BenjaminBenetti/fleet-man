//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// TestLiveDocker runs the docker engines of instance pods on a real cluster:
// a docker-in-docker instance (the docker-in-docker feature: a privileged
// dev container running its own dockerd) and a docker-outside-of-docker one
// (the feature binds the engine's socket, which the pod's fleet-docker
// sidecar serves). Each pulls and runs containers at the pod network's MTU,
// in networks from the connection's address pool, with the engine's
// containers nested in the pod's own cgroups, and keeps its images across a
// stop and start; the DooD engine reloads when its shared daemon.json
// changes, and its siblings see the workspace. On a connection that does not
// allow privileged pods, the DooD create fails before the build. It needs
// FLEET_KUBE_TEST_CONTEXT (a cluster whose nodes reach Docker Hub), docker,
// the devcontainer CLI and kubectl; namespaces fleet-it-p5-03-* are deleted
// when it ends. FLEET_KUBE_TEST_DOCKER=dind or dood runs one of them.
func TestLiveDocker(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run docker engines in instance pods on a cluster")
	}
	for _, tool := range []string{"docker", "devcontainer", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	only := os.Getenv("FLEET_KUBE_TEST_DOCKER")
	lb := newLiveBackend(t, kctx)
	lb.prefix = "fleet-it-p5-03-dind-mtu-"
	const pool = "10.231.0.0/16"
	open := lb.connect(t, nil, true, func(rec *fleet.KubeCluster) { rec.DinDAddressPool = pool })

	if only == "" || only == "dind" {
		t.Run("dind", func(t *testing.T) { lb.dind(t, open) })
	}
	if only == "" || only == "dood" {
		t.Run("dood", func(t *testing.T) { lb.dood(t, open) })
		t.Run("dood refused", func(t *testing.T) {
			off := false
			closed := lb.connect(t, nil, true, func(rec *fleet.KubeCluster) { rec.AllowPrivileged = &off })
			lb.doodRefused(t, closed)
		})
	}
}

// dockerInstance creates instance name from integration fixture fx on lv.
func (lb *liveBackend) dockerInstance(t *testing.T, lv *live, name, fx string) (*fleet.Instance, *backend.UpResult, *Backend) {
	t.Helper()
	src := filepath.Join("..", "..", "..", "integration", fx)
	inst, _ := lb.instance(t, lv, name, func(ws string) {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", src+"/.", ws).CombinedOutput(); err != nil {
			t.Fatalf("copying %s: %v\n%s", fx, err, out)
		}
	})
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err != nil {
		t.Fatalf("create from %s: %v", fx, err)
	}
	t.Logf("created %s from %s in %v", res.ContainerID, fx, time.Since(start).Round(time.Second))
	inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
	return inst, res, newHandle(inst, lb.pool)
}

// shell runs scripts in an instance as its remote user.
type shell struct {
	t  *testing.T
	b  *Backend
	id string
}

func (s shell) run(script string) (string, error) {
	out, err := s.b.RunScript(s.id, script)
	return strings.TrimSpace(out), err
}

func (s shell) must(script string) string {
	s.t.Helper()
	out, err := s.run(script)
	if err != nil {
		s.t.Fatalf("%q: %v\n%s", script, err, out)
	}
	return out
}

// waitDocker waits until the instance's docker answers, and has the image
// the checks run (scripts' stderr, a pull's progress, is in their output).
func (s shell) waitDocker(when string) {
	s.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := s.run("docker info --format '{{.ServerVersion}}'")
		if err == nil && out != "" {
			s.must("docker pull -q busybox:1.36 >/dev/null 2>&1")
			return
		}
		if time.Now().After(deadline) {
			diag, _ := s.run("id; ls -la /var/run/docker.sock /var/run/docker-host.sock /opt/fleet/docker; sudo cat /tmp/vscr-docker-from-docker.log /tmp/dockerd.log 2>&1 | tail -n 20; docker info 2>&1 | tail -n 5")
			s.t.Fatalf("docker never answered %s: %v\n%s\n%s", when, err, out, diag)
		}
		time.Sleep(2 * time.Second)
	}
}

// checkNetworks: the engine's bridge runs at the pod network's MTU, and a
// network it creates takes a subnet of the connection's pool.
func (s shell) checkNetworks(pool string) {
	s.t.Helper()
	podMTU := s.must("cat /sys/class/net/eth0/mtu")
	if got := s.must(`docker network inspect bridge --format '{{index .Options "com.docker.network.driver.mtu"}}'`); got != podMTU {
		s.t.Errorf("the engine's bridge MTU %q, want the pod network's %s", got, podMTU)
	}
	s.must("docker network create fleet-live-net >/dev/null")
	subnet := s.must("docker network inspect fleet-live-net --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'")
	if !strings.HasPrefix(subnet, strings.TrimSuffix(pool, "0.0/16")) || !strings.HasSuffix(subnet, "/24") {
		s.t.Errorf("a new network's subnet %q, want a /24 of %s", subnet, pool)
	}
	s.must("docker network rm fleet-live-net >/dev/null")
	s.t.Logf("pod MTU %s; a new network got %s", podMTU, subnet)
}

func (lb *liveBackend) dind(t *testing.T, lv *live) {
	inst, res, b := lb.dockerInstance(t, lv, "dind", "fixture-dind")
	sh := shell{t: t, b: b, id: res.ContainerID}
	sh.waitDocker("after the create")
	sh.checkNetworks("10.231.0.0/16")

	// The engine runs in fleet-user and nests its containers there, beside
	// the user's processes: below the pod's limits, frozen with them.
	dockerd := sh.must(`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = dockerd ] && sed -n 's/^0:://p' $p/cgroup && break; done`)
	if !strings.HasSuffix(dockerd, "/fleet-user/procs") {
		t.Errorf("dockerd runs in %q, want fleet-user/procs", dockerd)
	}
	cfg := sh.must("sudo cat /etc/docker/daemon.json")
	parent := strings.TrimSuffix(dockerd, "/procs") + "/docker"
	if !strings.Contains(cfg, `"cgroup-parent": "`+parent+`"`) || !strings.Contains(cfg, `"mtu": `) || !strings.Contains(cfg, "10.231.0.0/16") {
		t.Errorf("daemon.json %s, want the MTU, the pool and the parent %s", cfg, parent)
	}
	// A limit works (the nested cgroups have their controllers), and the
	// container's cgroup is below the parent.
	inner := sh.must("docker run --rm -m 64m busybox:1.36 sh -c 'cat /sys/fs/cgroup/memory.max; cat /proc/self/cgroup'")
	if !strings.Contains(inner, "67108864") {
		t.Errorf("a container with -m 64m: %q", inner)
	}
	if got := sh.must(`ls -d /sys/fs/cgroup` + parent + `/* 2>/dev/null | head -n 1`); got == "" {
		t.Errorf("no container cgroup below %s", parent)
	}
	sh.must("docker tag busybox:1.36 fleet-live/kept:v1")
	id := sh.must("docker image inspect --format '{{.Id}}' fleet-live/kept:v1")

	if err := b.Stop(res.ContainerID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := b.Start(res.ContainerID); err != nil {
		t.Fatalf("start: %v", err)
	}
	sh.waitDocker("after a stop and start")
	if got := sh.must("docker image inspect --format '{{.Id}}' fleet-live/kept:v1"); got != id {
		t.Errorf("the image after a stop and start: %q, want %q (it lives on the instance disk)", got, id)
	}
	sh.checkNetworks("10.231.0.0/16")
	if err := b.Down(res.ContainerID); err != nil {
		t.Fatalf("down: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
}

func (lb *liveBackend) dood(t *testing.T, lv *live) {
	inst, res, b := lb.dockerInstance(t, lv, "dood", "fixture-dood")
	if w := strings.Join(res.Warnings, "\n"); !strings.Contains(w, "pod-local") {
		t.Errorf("the create's warnings %q do not say siblings are pod-local", w)
	}
	sh := shell{t: t, b: b, id: res.ContainerID}
	sh.waitDocker("after the create")
	if got := sh.must("readlink /var/run/docker-host.sock"); got != podspec.DockerSocket {
		t.Errorf("the bind's target links to %q, want %s", got, podspec.DockerSocket)
	}
	// The remote user reaches the engine as on a Linux docker host: the
	// link has the socket's group, which the feature gives its docker
	// group, so the user needs no socat proxy (which cuts a stream 0.5s
	// after one side closes, losing a quick container's output).
	if got := sh.must("id -un"); got != "vscode" {
		t.Errorf("scripts run as %s", got)
	}
	sockGID := sh.must("stat -L -c %g /var/run/docker-host.sock")
	if got := sh.must("stat -c %g /var/run/docker-host.sock"); got != sockGID || got == "0" {
		t.Errorf("the link's group %s, the socket's %s", got, sockGID)
	}
	if got := sh.must("id -G"); !strings.Contains(" "+got+" ", " "+sockGID+" ") {
		t.Errorf("the remote user's groups %q lack the socket's %s", got, sockGID)
	}
	if out, err := sh.run("pgrep -x socat"); err == nil {
		t.Errorf("a socat proxy runs: %s", out)
	}
	sh.checkNetworks("10.231.0.0/16")
	// A sibling sees the workspace, at the dev container's path.
	ws := res.RemoteWorkspaceFolder
	sh.must("cd " + ws + " && echo from-the-workspace > sibling-check")
	for i := 0; i < 5; i++ {
		if got := sh.must(`cd ` + ws + ` && docker run --rm -v "$PWD":/w busybox:1.36 cat /w/sibling-check`); got != "from-the-workspace" {
			t.Errorf("run %d: a sibling's bind of the workspace reads %q", i, got)
		}
	}
	// And at the daemon host's path, which ${localWorkspaceFolder} names.
	if got := sh.must(`docker run --rm -v ` + inst.WorkspaceDir + `:/w busybox:1.36 cat /w/sibling-check`); got != "from-the-workspace" {
		t.Errorf("a sibling's bind of the daemon host's workspace path reads %q", got)
	}
	if got := sh.must("docker run --rm -m 64m busybox:1.36 cat /sys/fs/cgroup/memory.max"); got != "67108864" {
		t.Errorf("a sibling with -m 64m: %q", got)
	}

	// The shared daemon.json: a change reloads the engine.
	cfgFile := sh.must("printenv " + podspec.DockerConfigEnv)
	if cfgFile != podspec.DockerConfig {
		t.Fatalf("%s=%q", podspec.DockerConfigEnv, cfgFile)
	}
	sh.must(fmt.Sprintf(`echo '{"registry-mirrors":["http://fleet-live-mirror.invalid:5000"]}' | sudo tee %s >/dev/null`, cfgFile))
	deadline := time.Now().Add(30 * time.Second)
	for {
		if out, _ := sh.run("docker info --format '{{.RegistryConfig.Mirrors}}'"); strings.Contains(out, "fleet-live-mirror.invalid") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine did not reload its daemon.json")
		}
		time.Sleep(time.Second)
	}
	sh.must(fmt.Sprintf(`echo '{}' | sudo tee %s >/dev/null`, cfgFile))

	// The engine's own cgroup nesting, seen from the sidecar.
	pod := names.Pod(names.Instance("live", "dood", inst.KubeKey))
	kubectl := func(args ...string) string {
		cmd := exec.Command("kubectl", append([]string{"--context", lb.kctx, "-n", lv.ns}, args...)...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+strings.Join(lv.rec.KubeconfigPaths, string(os.PathListSeparator)))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	ps := kubectl("exec", pod, "-c", podspec.DockerSidecar, "--", "sh", "-c", `for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = dockerd ] && tr '\0' ' ' < $p/cmdline && sed -n 's/^0:://p' $p/cgroup; done; true`)
	if !strings.Contains(ps, "--cgroup-parent=") || !strings.Contains(ps, "--mtu=") || !strings.Contains(ps, "/init") || strings.Contains(ps, "tcp://") {
		t.Errorf("the sidecar's dockerd: %q", ps)
	}
	t.Logf("sidecar dockerd: %s", ps)

	sh.must("docker tag busybox:1.36 fleet-live/kept:v1")
	id := sh.must("docker image inspect --format '{{.Id}}' fleet-live/kept:v1")
	if err := b.Stop(res.ContainerID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := b.Start(res.ContainerID); err != nil {
		t.Fatalf("start: %v", err)
	}
	sh.waitDocker("after a stop and start")
	if got := sh.must("docker image inspect --format '{{.Id}}' fleet-live/kept:v1"); got != id {
		t.Errorf("the image after a stop and start: %q, want %q (it lives on vol/fleet-docker)", got, id)
	}
	if err := b.Down(res.ContainerID); err != nil {
		t.Fatalf("down: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
}

// doodRefused: without privileged pods the create fails before the build,
// with the toggle named, and leaves nothing.
func (lb *liveBackend) doodRefused(t *testing.T, lv *live) {
	src := filepath.Join("..", "..", "..", "integration", "fixture-dood")
	inst, _ := lb.instance(t, lv, "doodoff", func(ws string) {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", src+"/.", ws).CombinedOutput(); err != nil {
			t.Fatalf("copying: %v\n%s", err, out)
		}
	})
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log := &stepLog{}
	_, err := b.UpContext(backend.WithJobLog(ctx, log), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err == nil || !strings.HasPrefix(err.Error(), podspec.CodeDooDNeedsPrivileged+": ") || !strings.Contains(err.Error(), "Allow privileged pods") {
		t.Fatalf("a DooD create without privileged pods: %v", err)
	}
	if strings.Contains(log.String(), "==> "+backend.StepLine(fleet.StepImageBuild, "")) {
		t.Errorf("the refused create built:\n%s", log)
	}
	if err := b.DownByKey(); err != nil {
		t.Fatalf("down by key: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
}

// stepLog keeps a job log.
type stepLog struct{ strings.Builder }
