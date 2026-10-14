//go:build linux && !nok8s

package podspec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// bootImage is the image the integration fixture's devcontainer names, which
// the test cluster's node holds.
const bootImage = "mcr.microsoft.com/devcontainers/base:debian-12"

// tierCUID is the arbitrary uid a restricted cluster runs a pod as.
const tierCUID = 1000650000

// TestLiveBoot boots an instance from the renderers' objects alone, with the
// real seed and pod-init: the rendered seed Job (staged and fed as fleetd
// does, by the jobboot protocol over kubectl exec) seeds the rendered disk,
// and the StatefulSet rendered over its manifest runs pod-init as PID 1 over
// the persisted root. It boots one as root in an unlabelled namespace and one
// as an arbitrary uid in a restricted one (capability tier C). It needs
// kubectl and a cluster whose node has bootImage (FLEET_KUBE_TEST_CONTEXT),
// and builds the pod binary.
func TestLiveBoot(t *testing.T) {
	c := newLiveCluster(t)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("needs kubectl")
	}
	bin, sha := buildLivePodBinary(t)
	t.Run("root", func(t *testing.T) {
		bootInstance(t, c, c.namespace(t, "boot", nil), Identity{}, bin, sha)
	})
	t.Run("tier C", func(t *testing.T) {
		ns := c.namespace(t, "boot-restricted", map[string]string{"pod-security.kubernetes.io/enforce": "restricted"})
		bootInstance(t, c, ns, Identity{NonRoot: true, UID: ptr(int64(tierCUID)), GID: ptr(int64(0)), FSGroup: ptr(int64(tierCUID))}, bin, sha)
	})
}

func bootInstance(t *testing.T, c *liveCluster, ns string, id Identity, bin, sha string) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	kubectl := func(stdin []byte, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", kctx, "-n", ns}, args...)...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			t.Fatalf("kubectl %s: %v\n%s%s", strings.Join(args, " "), err, out.String(), errOut.String())
		}
		return out.String()
	}
	in, r := liveInput(t, "fixture", ns, fleet.KubeCluster{})
	r.Image = ImageRef{Ref: bootImage, ID: "boot"}
	r.Root = RootLayout{}
	r.Identity = id
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	create := metav1.CreateOptions{FieldManager: FieldManager}
	if err := DryRun(ctx, c.api, o); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := c.api.CoreV1().PersistentVolumeClaims(ns).Create(ctx, o.Disk, create); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*corev1.Secret{o.SpecSecret, o.EnvSecret} {
		if _, err := c.api.CoreV1().Secrets(ns).Create(ctx, s, create); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.api.BatchV1().Jobs(ns).Create(ctx, o.Seed.Job, create); err != nil {
		t.Fatal(err)
	}
	kubectl(nil, "wait", "--for=condition=Ready", "pod", "-l", "job-name="+o.Seed.Job.Name, "--timeout=300s")
	seedPod := strings.TrimSpace(kubectl(nil, "get", "pod", "-l", "job-name="+o.Seed.Job.Name, "-o", "jsonpath={.items[0].metadata.name}"))

	// Stage the binary and the job spec, as fleetd does.
	probe, err := jobboot.ParseProbe(kubectl(nil, "exec", seedPod, "--", "sh", "-c", jobboot.ProbeScript))
	wantUID := 0
	if id.NonRoot {
		wantUID = tierCUID
	}
	if err != nil || probe.UID != wantUID {
		t.Fatalf("probe %+v %v", probe, err)
	}
	paths := jobboot.Paths{}
	data, _ := os.ReadFile(bin)
	wire := data
	f := jobboot.StageFile{Path: paths.Binary(), Size: int64(len(data)), SHA256: sha, Mode: 0o755, CheckSHA: probe.SHA256Sum}
	if probe.Gzip {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(data)
		_ = zw.Close()
		wire, f.Gzip, f.WireSize = gz.Bytes(), true, int64(gz.Len())
	}
	script, err := jobboot.StageScript(f)
	if err != nil {
		t.Fatal(err)
	}
	kubectl(wire, "exec", "-i", seedPod, "--", "sh", "-c", script)
	seed := SeedSpec(res, o)
	seed.Workspace = &podseed.Workspace{}
	job := podseed.JobSpec{V: podseed.JobSpecVersion, Op: podseed.OpSeed, Gen: r.Gen, BinarySHA256: sha, PVC: JobDiskPath, Seed: seed}
	if err := job.Validate(); err != nil {
		t.Fatalf("the seed spec from the render: %v", err)
	}
	specData, _ := json.Marshal(job)
	script, _ = jobboot.StageScript(jobboot.StageFile{Path: paths.Spec(), Size: int64(len(specData)), SHA256: liveSHA(specData), Ready: true, CheckSHA: probe.SHA256Sum})
	kubectl(specData, "exec", "-i", seedPod, "--", "sh", "-c", script)

	feed := exec.CommandContext(ctx, "kubectl", "--context", kctx, "-n", ns, "exec", "-i", seedPod, "--", paths.Binary(), "pod-job", "feed")
	stdin, _ := feed.StdinPipe()
	stdout, _ := feed.StdoutPipe()
	if err := feed.Start(); err != nil {
		t.Fatal(err)
	}
	result, err := jobboot.Feed(ctx, struct {
		io.Reader
		io.Writer
	}{stdout, stdin}, jobboot.FeedOptions{Workspace: bytes.NewReader(liveTar(t, map[string]string{"README.md": "# booted\n"}))})
	_ = stdin.Close()
	_ = feed.Wait()
	if err != nil || !result.OK {
		t.Fatalf("seed: %v %s\n%s", err, result.Error, kubectl(nil, "logs", seedPod))
	}
	m, err := podseed.ParseManifest(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded dirs %v, root-extra %v, %d files", m.Dirs, m.RootExtra, m.Files)

	// The instance, rendered over what the seed made.
	r.Root = RootLayout{Dirs: m.Dirs, Extra: m.RootExtra}
	o, err = Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.api.AppsV1().StatefulSets(ns).Create(ctx, o.StatefulSet, create); err != nil {
		t.Fatal(err)
	}
	pod := names.Pod(o.StatefulSet.Name)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		p, err := c.api.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
		if err == nil && podReady(p) {
			if got := SpecHash(&p.Spec); got != o.StatefulSet.Annotations[names.AnnotationSpecHash] {
				t.Errorf("the running pod hashes %s, its StatefulSet says %s", got, o.StatefulSet.Annotations[names.AnnotationSpecHash])
			}
			if v := CheckHardened(p.ObjectMeta, p.Spec, r.hardenFor(res)); len(v) > 0 {
				t.Errorf("the running pod is not hardened: %v", v)
			}
			break
		}
		if time.Now().After(deadline) {
			desc := exec.Command("kubectl", "--context", kctx, "-n", ns, "describe", "pod", pod)
			out, _ := desc.CombinedOutput()
			logs, _ := exec.Command("kubectl", "--context", kctx, "-n", ns, "logs", pod).CombinedOutput()
			t.Fatalf("%s never became Ready\n%s\n%s", pod, out, logs)
		}
		time.Sleep(time.Second)
	}
	sh := func(script string) string {
		return strings.TrimSpace(kubectl(nil, "exec", pod, "-c", DevContainer, "--", "sh", "-c", script))
	}
	checks := map[string]string{
		`tr '\0' ' ' < /proc/1/cmdline`:                              strings.Join(Command(), " "),
		"hostname; cat /etc/hostname":                                pod + "\n" + pod,
		"getent hosts kubernetes.default.svc >/dev/null && echo dns": "dns",
		"cat " + in.WorkspaceFolder + "/README.md":                   "# booted",
		"readlink /usr/bin/fleet":                                    podseed.CurrentBinary,
		"grep -c 'Container started' /opt/fleet/log/pid1.log":        "1",
	}
	if id.NonRoot {
		// One uid for everything, which pod-init gave a passwd entry; the
		// spec Secret is the pod's fsGroup's to read.
		checks["id -u; id -un"] = "1000650000\nfleet"
		checks["stat -c '%a %g' /opt/fleet/spec/..data/pid1.json"] = "440 1000650000"
		checks["echo kept > /usr/local/marker && cat /usr/local/marker"] = "kept"
	} else {
		checks["stat -c %a /opt/fleet/spec/..data/pid1.json"] = "400"
		checks["id -u vscode"] = "1000"
	}
	if m.SingleUID != id.NonRoot {
		t.Errorf("manifest single_uid %v", m.SingleUID)
	}
	for script, want := range checks {
		if got := sh(script); got != want {
			t.Errorf("%s: %q, want %q", script, got, want)
		}
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// buildLivePodBinary builds the static pod flavor of fleet (-tags nok8s)
// for this machine's arch, with its sha256.
func buildLivePodBinary(t *testing.T) (string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fleet")
	cmd := exec.Command("go", "build", "-tags", "nok8s", "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the pod binary: %v\n%s", err, msg)
	}
	sha, err := podseed.FileSHA256(out)
	if err != nil {
		t.Fatal(err)
	}
	return out, sha
}

func liveSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func liveTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()})
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}
