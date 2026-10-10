//go:build linux

package execas

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestKubeExecAs runs `fleet exec-as` (the static pod build, -tags nok8s)
// the way fleetd's fallback does: a plain kube exec whose stdin carries the
// framed request and then the command's input. It needs a cluster, so it runs
// only when FLEET_EXECAS_KUBE_CONTEXT names a kube context; it creates two
// namespaces named fleet-it-p2-01-execas-*, one labelled restricted (a tier C
// pod), and deletes them when done. FLEET_EXECAS_KUBE_IMAGE picks the image
// (default: the devcontainers base image, which has a vscode user).
func TestKubeExecAs(t *testing.T) {
	kctx := os.Getenv("FLEET_EXECAS_KUBE_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_EXECAS_KUBE_CONTEXT=<context> to run exec-as in real pods")
	}
	image := os.Getenv("FLEET_EXECAS_KUBE_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	k := kubectl{context: kctx}
	bin := buildPodBinary(t)

	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p2-01-execas-" + hex.EncodeToString(suffix[:])
	nsC := ns + "-rs"
	for _, n := range []string{ns, nsC} {
		k.must(t, nil, "create", "namespace", n)
		t.Cleanup(func() { _, _, _ = k.run(nil, "delete", "namespace", n, "--wait=false") })
	}
	k.must(t, nil, "label", "namespace", nsC, "pod-security.kubernetes.io/enforce=restricted")

	k.must(t, []byte(podManifest("execas-root", image, "")), "apply", "-n", ns, "-f", "-")
	k.must(t, []byte(podManifest("execas-tierc", image, `
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000650000
    runAsGroup: 0
    seccompProfile: {type: RuntimeDefault}`)), "apply", "-n", nsC, "-f", "-")
	for _, p := range [][2]string{{ns, "execas-root"}, {nsC, "execas-tierc"}} {
		k.must(t, nil, "wait", "-n", p[0], "--for=condition=Ready", "pod/"+p[1], "--timeout=180s")
	}

	spec, _ := json.Marshal(Spec{
		RemoteUser:      "vscode",
		WorkspaceFolder: "/tmp/ws",
		WorkingDir:      "/",
		RemoteEnv:       map[string]string{"FROM_REMOTE": "${containerEnv:PATH}:/remote"},
		SSHAuthSock:     "/tmp/agent.sock",
	})
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	// Stage the binary and the spec as the supervisor stages them: over exec
	// stdin. The root pod also gets a group membership and a login-file
	// variable to find.
	setup := map[string]string{
		"execas-root": `groupadd -g 998 dockerish && usermod -aG dockerish vscode &&
for f in .profile .bash_profile .zshenv; do echo 'export PROBED_FROM_PROFILE=1' >> /home/vscode/$f; done &&
chown vscode: /home/vscode/.profile /home/vscode/.bash_profile /home/vscode/.zshenv && install -d -o vscode /tmp/ws`,
		"execas-tierc": `mkdir -p /tmp/ws`,
	}
	for _, p := range [][2]string{{ns, "execas-root"}, {nsC, "execas-tierc"}} {
		k.must(t, data, "exec", "-i", "-n", p[0], p[1], "--", "sh", "-c", "cat > /tmp/fleet && chmod 0755 /tmp/fleet")
		k.must(t, spec, "exec", "-i", "-n", p[0], p[1], "--", "sh", "-c", "cat > /tmp/exec.json")
		k.must(t, nil, "exec", "-n", p[0], p[1], "--", "sh", "-c", setup[p[1]])
	}

	execAs := func(ns, pod string, req Request, input string) (string, string, int) {
		t.Helper()
		var stdin bytes.Buffer
		if err := WriteRequest(&stdin, req); err != nil {
			t.Fatal(err)
		}
		stdin.WriteString(input)
		return k.run(stdin.Bytes(), "exec", "-i", "-n", ns, pod, "--",
			"/tmp/fleet", "exec-as", "--spec", "/tmp/exec.json", "--cache-dir", "/tmp/execas-cache")
	}
	report := []string{"sh", "-c", `id -u; id -g; id -G; pwd; echo "P=$PROBED_FROM_PROFILE H=$HOME S=$SSH_AUTH_SOCK R=$FROM_REMOTE"; cat`}

	t.Run("root pod exec class is devcontainer exec", func(t *testing.T) {
		out, errOut, code := execAs(ns, "execas-root", Request{Class: ClassExec, Argv: report}, "stdin after the frame")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		got := lines(out)
		if len(got) < 6 || got[0] != "1000" || got[1] != "1000" || !strings.Contains(" "+got[2]+" ", " 998 ") || got[3] != "/tmp/ws" {
			t.Fatalf("identity/cwd lines: %q", got)
		}
		if !strings.HasPrefix(got[4], "P=1 H=/home/vscode S=/tmp/agent.sock R=/") || !strings.HasSuffix(got[4], ":/remote") {
			t.Fatalf("env line: %q", got[4])
		}
		if got[5] != "stdin after the frame" {
			t.Fatalf("stdin: %q", got[5])
		}
		// The probe went through the pod's private cache.
		ls, _, _ := k.run(nil, "exec", "-n", ns, "execas-root", "--", "sh", "-c", "stat -c '%U %a' /tmp/execas-cache; ls /tmp/execas-cache")
		if !strings.HasPrefix(ls, "root 700\nprobe-") {
			t.Fatalf("probe cache: %q", ls)
		}
	})

	t.Run("root pod script and root classes", func(t *testing.T) {
		out, errOut, code := execAs(ns, "execas-root", Request{Class: ClassScript, Argv: report}, "")
		if got := lines(out); code != 0 || len(got) < 5 || got[0] != "1000" || got[3] != "/" || got[4] != "P= H=/home/vscode S= R=" {
			t.Fatalf("script: exit %d out %q err %q", code, out, errOut)
		}
		out, errOut, code = execAs(ns, "execas-root", Request{Class: ClassRoot, Argv: report}, "")
		if got := lines(out); code != 0 || len(got) < 5 || got[0] != "0" || !strings.Contains(got[4], "H=/root") {
			t.Fatalf("root: exit %d out %q err %q", code, out, errOut)
		}
	})

	t.Run("exit codes cross the kube exec", func(t *testing.T) {
		if _, _, code := execAs(ns, "execas-root", Request{Class: ClassScript, Argv: []string{"sh", "-c", "exit 7"}}, ""); code != 7 {
			t.Fatalf("exit %d, want 7", code)
		}
		_, errOut, code := execAs(ns, "execas-root", Request{Class: ClassScript, Argv: []string{"no-such-tool"}}, "")
		if code != 127 || !strings.Contains(errOut, `fleet exec-as: exec: "no-such-tool": executable file not found`) {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
		_, errOut, code = execAs(ns, "execas-root", Request{Class: ClassExplicit, Argv: []string{"true"}, User: "ghost"}, "")
		if code != 126 || !strings.Contains(errOut, "unable to find user ghost") {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
	})

	t.Run("tier C pod runs every class as the pod uid", func(t *testing.T) {
		for _, req := range []Request{
			{Class: ClassExec, Argv: report},
			{Class: ClassRoot, Argv: report},
			{Class: ClassExplicit, Argv: report, User: "vscode"},
		} {
			out, errOut, code := execAs(nsC, "execas-tierc", req, "")
			got := lines(out)
			if code != 0 || len(got) < 5 || got[0] != "1000650000" || got[1] != "0" {
				t.Fatalf("%s: exit %d out %q err %q", req.Class, code, out, errOut)
			}
			if !strings.Contains(got[4], "H=/ ") {
				t.Errorf("%s: a uid with no passwd entry has HOME /: %q", req.Class, got[4])
			}
		}
	})
}

// kubectl runs kubectl against one context.
type kubectl struct{ context string }

func (k kubectl) run(stdin []byte, args ...string) (string, string, int) {
	cmd := exec.Command("kubectl", append([]string{"--context", k.context}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

func (k kubectl) must(t *testing.T, stdin []byte, args ...string) string {
	t.Helper()
	out, errOut, code := k.run(stdin, args...)
	if code != 0 {
		t.Fatalf("kubectl %s: exit %d: %s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func podManifest(name, image, podSecurity string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  terminationGracePeriodSeconds: 1%s
  containers:
  - name: dev
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sleep", "infinity"]
    securityContext:
      allowPrivilegeEscalation: %v
      capabilities: {drop: [%s]}
`, name, podSecurity, image, podSecurity == "", map[bool]string{true: "", false: "ALL"}[podSecurity == ""])
}

// buildPodBinary builds the static pod flavor of fleet for the cluster.
func buildPodBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fleet")
	cmd := exec.Command("go", "build", "-tags", "nok8s", "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+envOr("FLEET_EXECAS_KUBE_ARCH", runtime.GOARCH))
	start := time.Now()
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the pod binary: %v\n%s", err, msg)
	}
	t.Logf("built the pod binary in %s", time.Since(start).Round(time.Second))
	return out
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
