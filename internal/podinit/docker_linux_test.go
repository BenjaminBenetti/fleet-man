//go:build linux

package podinit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// TestPodInitAsPID1InDocker runs the static pod binary as PID 1 of a
// privileged docker container with a memory limit, in a private cgroup
// namespace (what cri-dockerd gives a privileged pod) and in the host's (what
// containerd and CRI-O give one), and checks the cgroup layout, that an OOM in
// fleet-user kills one process and not the container, the dropped privileges,
// the rlimits, the stop signal and the rolling output file. It needs docker,
// so it runs only with FLEET_PODINIT_DOCKER=1 (FLEET_PODINIT_DOCKER_IMAGE
// picks the image, default debian:bookworm-slim).
func TestPodInitAsPID1InDocker(t *testing.T) {
	if os.Getenv("FLEET_PODINIT_DOCKER") == "" {
		t.Skip("set FLEET_PODINIT_DOCKER=1 to run pod-init as PID 1 in docker")
	}
	image := os.Getenv("FLEET_PODINIT_DOCKER_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	bin := buildPodBinary(t)
	for _, cgroupns := range []string{"private", "host"} {
		t.Run(cgroupns, func(t *testing.T) { runPID1InDocker(t, bin, image, cgroupns) })
	}
}

func runPID1InDocker(t *testing.T, bin, image, cgroupns string) {
	fleet := t.TempDir()
	for _, d := range []string{"bin", "log", "run", "spec"} {
		if err := os.MkdirAll(filepath.Join(fleet, d), 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(fleet, d), 0o777)
	}
	sha, err := podseed.FileSHA256(bin)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(bin)
	if err := os.WriteFile(filepath.Join(fleet, "bin", podseed.BinaryName(sha)), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(podseed.BinaryName(sha), filepath.Join(fleet, "bin", "fleet")); err != nil {
		t.Fatal(err)
	}
	script := `echo "uid=$(id -u)"
echo "self=$(sed -n 's/^0:://p' /proc/self/cgroup)"
echo "pid1=$(sed -n 's/^0:://p' /proc/1/cgroup)"
C=$(sed -n 's/^0:://p' /proc/1/cgroup); C=/sys/fs/cgroup${C%/fleet-agent}
echo "oom=$(cat $C/memory.oom.group)"
echo "usermax=$(cat $C/fleet-user/memory.max)"
echo "nofile=$(ulimit -n)"
trap 'echo got-int; exit 3' INT
echo "ready $$"
while :; do sleep 1 & wait $!; done`
	spec := Spec{Format: SpecVersion, Argv: []string{"/bin/sh", "-c", script}, User: "nobody", StopSignal: "SIGINT",
		Rlimits: []Rlimit{{Name: "nofile", Soft: 2048, Hard: 2048}}, BinarySHA256: sha}
	specData, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(fleet, "spec", "pid1.json"), specData, 0o644); err != nil {
		t.Fatal(err)
	}

	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	name := "fleet-podinit-test-" + hex.EncodeToString(suffix[:])
	docker(t, "run", "-d", "--name", name, "--privileged", "--cgroupns="+cgroupns, "-m", "512m",
		"-v", fleet+":/opt/fleet", image, "/opt/fleet/bin/fleet", "pod-init", "--spec", "/opt/fleet/spec/pid1.json")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	logs := waitDockerLog(t, name, "ready")
	want := map[string]string{"uid": "65534", "pid1": "/fleet-agent", "self": "/fleet-user/procs", "oom": "0",
		"usermax": "469762048", "nofile": "2048"}
	got := map[string]string{}
	for _, line := range strings.Split(logs, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	for k, v := range want {
		if k == "pid1" || k == "self" {
			if !strings.HasSuffix(got[k], v) {
				t.Errorf("%s cgroup %q, want it to end in %s", k, got[k], v)
			}
			continue
		}
		if got[k] != v {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], v, logs)
		}
	}
	var layout Layout
	if data, err := os.ReadFile(filepath.Join(fleet, "run", "cgroup.json")); err != nil || json.Unmarshal(data, &layout) != nil || !layout.OOMGroup {
		t.Errorf("cgroup.json: %s %v", data, err)
	}

	// An OOM in fleet-user kills the hog, not the container. (A docker exec
	// lands in PID 1's cgroup, fleet-agent; the hog moves itself into
	// fleet-user's process cgroup, as the agent moves a user's.) The kernel
	// may pick a second victim while the first one's memory is still being
	// freed, so the test looks at the outcome, not the hog's own output.
	hog := `C=$(sed -n 's/^0:://p' /proc/1/cgroup); C=/sys/fs/cgroup${C%/fleet-agent}
echo $$ > $C/fleet-user/procs/cgroup.procs
head -c 900m /dev/zero | tail > /dev/null`
	_ = exec.Command("docker", "exec", name, "sh", "-c", hog).Run()
	events := docker(t, "exec", name, "sh", "-c", `C=$(sed -n 's/^0:://p' /proc/1/cgroup); cat /sys/fs/cgroup${C%/fleet-agent}/fleet-user/memory.events`)
	if !strings.Contains(events, "oom_kill ") || strings.Contains(events, "oom_kill 0") {
		t.Errorf("the hog did not hit fleet-user's limit:\n%s", events)
	}
	// (docker's OOMKilled flag counts any kill in the container's cgroup tree,
	// so only Running says whether the container survived.)
	if running := strings.TrimSpace(docker(t, "inspect", "-f", "{{.State.Running}}", name)); running != "true" {
		t.Fatalf("after the OOM the container is running=%s, want it still running", running)
	}
	scriptPID := strings.TrimPrefix(strings.TrimSpace(logs[strings.Index(logs, "ready "):]), "ready ")
	if strings.ContainsAny(scriptPID, "\n ") {
		scriptPID = strings.Fields(scriptPID)[0]
	}
	if out, err := exec.Command("docker", "exec", name, "sh", "-c", "kill -0 "+scriptPID).CombinedOutput(); err != nil {
		t.Logf("the OOM took the start script too (a second victim): %s", out)
		return
	}

	// docker stop sends SIGTERM; the script gets its stop signal, SIGINT.
	docker(t, "stop", "-t", "20", name)
	if code := strings.TrimSpace(docker(t, "inspect", "-f", "{{.State.ExitCode}}", name)); code != "3" {
		t.Errorf("exit code %s, want the script's 3 from its INT trap\n%s", code, docker(t, "logs", name))
	}
	// The rolling file is root's (0600); open it up to read it here.
	docker(t, "run", "--rm", "-v", fleet+":/opt/fleet", image, "chmod", "-R", "a+rX", "/opt/fleet/log")
	runs, err := ReadRolling(filepath.Join(fleet, "log"))
	if err != nil || len(runs) != 1 || !strings.Contains(string(runs[0].Data), "ready") || !strings.Contains(string(runs[0].Data), "got-int") {
		t.Errorf("rolling file: %+v %v", runs, err)
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func waitDockerLog(t *testing.T, name, want string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Contains(string(out), want) {
			return string(out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the container log:\n%s", want, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// buildPodBinary builds the static fleet-pod binary (-tags nok8s).
func buildPodBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fleet")
	cmd := exec.Command("go", "build", "-tags", "nok8s", "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the pod binary: %v\n%s", err, msg)
	}
	return out
}
