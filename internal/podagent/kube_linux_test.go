//go:build linux

package podagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// TestKubePodAgent runs the pod build of fleet (-tags nok8s) in a real pod
// the way fleetd does: `pod-agent serve` over a kube exec's stdio, and a
// direct-mode `pod-agent exec --stdio`. It cuts each exec by killing kubectl
// (a client that vanishes, as a laptop that sleeps), over WebSocket and over
// SPDY (containerd kills an exec's process when an SPDY stream is cut), and
// checks the held command comes back through a new exec. It runs only when
// FLEET_PODAGENT_KUBE_CONTEXT names a kube context, in a namespace named
// fleet-it-p2-02-podagent-*, deleted when done. FLEET_PODAGENT_KUBE_IMAGE
// picks the image (default: the devcontainers base image, with its vscode
// user).
func TestKubePodAgent(t *testing.T) {
	kctx := os.Getenv("FLEET_PODAGENT_KUBE_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_PODAGENT_KUBE_CONTEXT=<context> to run the pod agent in a real pod")
	}
	image := os.Getenv("FLEET_PODAGENT_KUBE_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	k := kubeCtl{context: kctx}
	bin := buildFleetPod(t)
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p2-02-podagent-" + hex.EncodeToString(suffix[:])
	k.must(t, nil, "create", "namespace", ns)
	t.Cleanup(func() { _, _, _ = k.run(nil, "delete", "namespace", ns, "--wait=false") })
	k.must(t, []byte(agentPodManifest("agent", image)), "apply", "-n", ns, "-f", "-")
	k.must(t, nil, "wait", "-n", ns, "--for=condition=Ready", "pod/agent", "--timeout=180s")
	podUID := strings.TrimSpace(k.must(t, nil, "get", "pod", "-n", ns, "agent", "-o", "jsonpath={.metadata.uid}"))

	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(execas.Spec{RemoteUser: "vscode", WorkspaceFolder: "/home/vscode", WorkingDir: "/"})
	k.must(t, data, "exec", "-i", "-n", ns, "agent", "--", "sh", "-c", "mkdir -p /opt/fleet/bin /opt/fleet/spec && cat > /opt/fleet/bin/fleet && chmod 0755 /opt/fleet/bin/fleet")
	k.must(t, spec, "exec", "-i", "-n", ns, "agent", "--", "sh", "-c", "cat > /opt/fleet/spec/exec.json")

	for _, transport := range []string{"websocket", "spdy"} {
		t.Run("serve over "+transport, func(t *testing.T) {
			env := kubeExecEnv(transport)
			s1 := k.serve(t, ns, env, podUID)
			ctx := context.Background()

			res, err := s1.c.Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh", "-c", "id -un; pwd"}}}, nil)
			if err != nil || string(res.Stdout) != "vscode\n/home/vscode\n" {
				t.Fatalf("exec class: %+v %v", res, err)
			}
			blob := bytes.Repeat([]byte("pod-file!"), 7000)
			path := "/home/vscode/podwire-" + transport
			if err := s1.c.WriteFile(ctx, podwire.FileWriteHeader{Path: path, Mode: 0o600, Size: int64(len(blob))}, bytes.NewReader(blob)); err != nil {
				t.Fatal(err)
			}
			if got, err := s1.c.ReadFile(ctx, podwire.FileReadHeader{Path: path}); err != nil || !bytes.Equal(got, blob) {
				t.Fatalf("read back %d bytes, %v", len(got), err)
			}
			res, err = s1.c.Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"stat", "-c", "%U %a", path}}}, nil)
			if err != nil || strings.TrimSpace(string(res.Stdout)) != "vscode 600" {
				t.Fatalf("file owner: %+v %v", res, err)
			}

			e, err := s1.c.Exec(ctx, resumable(podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", `while read l; do echo "got:$l"; done`}}}))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = e.Stdin().Write([]byte("one\n"))
			readUntil(t, e.Stdout(), "got:one", 10*time.Second)
			pid := e.PID()

			oldAgent := s1.c.Hello().PID
			rss, _, _ := k.run(nil, "exec", "-n", ns, "agent", "--", "sh", "-c", fmt.Sprintf(
				`echo "agent $(grep VmRSS /proc/%d/status)"; for p in /proc/[0-9]*; do [ "${p#/proc/}" = "$$" ] && continue; tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q '^fleet pod-agent shepherd' && echo "shepherd $(grep VmRSS $p/status)"; done`, oldAgent))
			t.Logf("RSS over %s:\n%s", transport, strings.TrimSpace(rss))
			s1.kill()
			waitDetached(t, e)
			time.Sleep(time.Second)
			survived, _, _ := k.run(nil, "exec", "-n", ns, "agent", "--", "sh", "-c", fmt.Sprintf("test -d /proc/%d && echo alive || echo gone", oldAgent))
			t.Logf("after the %s cut the old agent (pid %d) is %s; the held command (pid %d) is held by its shepherd", transport, oldAgent, strings.TrimSpace(survived), pid)
			s2 := k.serve(t, ns, env, podUID)
			if err := s2.c.Reattach(ctx, e); err != nil {
				t.Fatalf("re-attach after a cut over %s: %v", transport, err)
			}
			_, _ = e.Stdin().Write([]byte("two\n"))
			readUntil(t, e.Stdout(), "got:two", 10*time.Second)
			_ = e.Stdin().Close()
			if code, err := e.Wait(); code != 0 || err != nil {
				t.Fatalf("code %d %v", code, err)
			}
			alive, _, _ := k.run(nil, "exec", "-n", ns, "agent", "--", "sh", "-c", fmt.Sprintf("test -d /proc/%d && echo alive", pid))
			if strings.Contains(alive, "alive") {
				t.Fatal("the command outlived its end")
			}
			// The new agent took over: the pid file names it.
			rec := k.must(t, nil, "exec", "-n", ns, "agent", "--", "cat", "/fleet-mounts/control/.pod-agent.pid")
			if !strings.Contains(rec, fmt.Sprintf(`"pid":%d`, s2.c.Hello().PID)) {
				t.Fatalf("pid file %q, new agent pid %d", rec, s2.c.Hello().PID)
			}
			s2.kill()
		})

		t.Run("exec --stdio over "+transport, func(t *testing.T) {
			env := kubeExecEnv(transport)
			x1 := k.execStdio(t, ns, env)
			h := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh", "-c", `stty size; while read l; do echo "got:$l"; done`}, TTY: true, Rows: 30, Cols: 100}}
			h.Resumable, h.PodUID = true, podUID
			ctx := context.Background()
			e, err := podwire.OpenExec(ctx, x1, h, podwire.ExecOptions{Heartbeat: true})
			if err != nil {
				t.Fatal(err)
			}
			out := collect(e.Stdout())
			out.waitFor(t, func(s string) bool { return strings.Contains(s, "30 100") }, 10*time.Second)
			_, _ = e.Stdin().Write([]byte("a\r"))
			out.waitFor(t, func(s string) bool { return strings.Contains(s, "got:a") }, 10*time.Second)
			x1.kill()
			waitDetached(t, e)
			time.Sleep(time.Second)
			shell, _, _ := k.run(nil, "exec", "-n", ns, "agent", "--", "sh", "-c", fmt.Sprintf("test -d /proc/%d && echo alive || echo gone", e.PID()))
			t.Logf("after the %s cut the attached shell (pid %d) is %s, held by its shepherd", transport, e.PID(), strings.TrimSpace(shell))
			x2 := k.execStdio(t, ns, env)
			if err := e.ReattachOver(ctx, x2); err != nil {
				t.Fatalf("re-attach after a cut over %s: %v", transport, err)
			}
			_, _ = e.Stdin().Write([]byte("b\r"))
			got := out.waitFor(t, func(s string) bool { return strings.Contains(s, "got:b") }, 10*time.Second)
			if strings.Count(got, "got:a") != 1 {
				t.Fatalf("output %q", got)
			}
			_, _ = e.Stdin().Write([]byte{4})
			if code, err := e.Wait(); code != 0 || err != nil {
				t.Fatalf("code %d %v", code, err)
			}
		})
	}
}

// kubeExecEnv is kubectl's environment for one exec transport.
func kubeExecEnv(transport string) []string {
	return append(os.Environ(), "KUBECTL_REMOTE_COMMAND_WEBSOCKETS="+fmt.Sprint(transport == "websocket"))
}

type kubeCtl struct{ context string }

func (k kubeCtl) run(stdin []byte, args ...string) (string, string, int) {
	cmd := exec.Command("kubectl", append([]string{"--context", k.context}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

func (k kubeCtl) must(t *testing.T, stdin []byte, args ...string) string {
	t.Helper()
	out, errOut, code := k.run(stdin, args...)
	if code != 0 {
		t.Fatalf("kubectl %s: exit %d: %s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// kubeExec is a kubectl exec whose stdio is a transport.
type kubeExec struct {
	cmd *exec.Cmd
	io.Reader
	io.WriteCloser
	c *podwire.Client
}

func (k kubeCtl) start(t *testing.T, env []string, args ...string) *kubeExec {
	t.Helper()
	cmd := exec.Command("kubectl", append([]string{"--context", k.context}, args...)...)
	cmd.Env = env
	cmd.Stderr = testWriter{t}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	x := &kubeExec{cmd: cmd, Reader: stdout, WriteCloser: stdin}
	t.Cleanup(x.kill)
	return x
}

// kill ends kubectl at once: the exec stream is cut, not closed.
func (x *kubeExec) kill() {
	_ = x.cmd.Process.Signal(syscall.SIGKILL)
	_ = x.cmd.Wait()
}

// serve starts `pod-agent serve` in the pod and dials it, checking the agent
// reports the pod's own uid.
func (k kubeCtl) serve(t *testing.T, ns string, env []string, podUID string) *kubeExec {
	t.Helper()
	x := k.start(t, env, "exec", "-i", "-n", ns, "agent", "--", "/opt/fleet/bin/fleet", "pod-agent", "serve", "--proto", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := podwire.Dial(ctx, x, podwire.ClientOptions{Version: "test", Check: func(h podwire.Hello) error {
		if h.PodUID != podUID {
			return fmt.Errorf("pod uid %q, want %q", h.PodUID, podUID)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	x.c = c
	if err := c.Call(ctx, "version", nil, nil); err != nil {
		t.Fatal(err)
	}
	return x
}

func (k kubeCtl) execStdio(t *testing.T, ns string, env []string) *kubeExec {
	t.Helper()
	return k.start(t, env, "exec", "-i", "-n", ns, "agent", "--", "/opt/fleet/bin/fleet", "pod-agent", "exec", "--stdio")
}

func agentPodManifest(name, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  terminationGracePeriodSeconds: 1
  containers:
  - name: dev
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sleep", "infinity"]
    env:
    - name: FLEET_POD_UID
      valueFrom: {fieldRef: {fieldPath: metadata.uid}}
    volumeMounts:
    - {name: control, mountPath: /fleet-mounts/control}
    - {name: run, mountPath: /opt/fleet/run}
  volumes:
  - {name: control, emptyDir: {}}
  - {name: run, emptyDir: {}}
`, name, image)
}

func buildFleetPod(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fleet")
	cmd := exec.Command("go", "build", "-tags", "nok8s", "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the pod binary: %v\n%s", err, msg)
	}
	return out
}
