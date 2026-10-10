//go:build !nok8s && linux

package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// TestLivePool runs the pool against a real cluster: an instance-shaped
// StatefulSet in a fleet-it-p2-10a-* namespace (deleted when done), the
// real pod build of fleet, the real exec transport. It runs only when
// FLEET_KUBE_TEST_CONTEXT names a context of the default kubeconfig.
// FLEET_KUBE_TEST_IMAGE picks the image (the devcontainers base by default);
// FLEET_TEST_TMUX_BUNDLE adds the bundled tmux.
func TestLivePool(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run the pool against a cluster")
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	ctx := context.Background()
	lv := newLive(t, kctx)
	v1 := lv.buildPod("vlive-1")

	ns := lv.ns
	sts, secret, renderHash := lv.applyInstance(image)
	pod := lv.waitRunning(sts)
	t.Logf("pod %s/%s runs %s (imageID %s)", ns, pod.Name, image, pod.Status.ContainerStatuses[0].ImageID)
	if imageDigest(pod.Status.ContainerStatuses[0].ImageID) == "" {
		t.Fatalf("the runtime's imageID %q carries no sha256 digest", pod.Status.ContainerStatuses[0].ImageID)
	}
	target := PodTarget{
		Cluster: lv.rec.ID, Fleet: "live", Instance: "one", StatefulSet: sts.Name, SpecSecret: secret,
		Priority: PriorityInteractive,
		Expect:   Expect{ImageDigests: []string{pod.Status.ContainerStatuses[0].ImageID}, SpecHash: renderHash, StatefulSetUID: sts.UID},
	}

	pool, execs := lv.pool(v1)
	s, err := pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if got := execs.snapshot(); got["probe"] != 1 || got["stage"] != 1 || got["serve"] != 1 {
		t.Fatalf("first bootstrap execs %v", got)
	}
	if h := s.Hello(); h.Version != "vlive-1" || h.PodUID != string(pod.UID) {
		t.Fatalf("hello %+v", h)
	}
	if out := run(t, s.Client(), "id -un; pwd"); out != "vscode\n/\n" {
		t.Fatalf("exec class ran as %q", out)
	}
	sha1 := shaOfFile(t, v1.Path)
	if spec := lv.pid1(secret); spec["binary_sha256"] != sha1 {
		t.Fatalf("pid1.json %v, want binary_sha256 %s", spec, sha1)
	}

	t.Run("bulk", func(t *testing.T) {
		bulk, err := s.Bulk(ctx)
		if err != nil {
			t.Fatal(err)
		}
		big := make([]byte, 6<<20)
		_, _ = rand.Read(big)
		sum := sha256.Sum256(big)
		start := time.Now()
		if err := bulk.WriteFile(ctx, podwire.FileWriteHeader{Path: "/tmp/big", Mode: 0o600, Size: int64(len(big)), SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(big)); err != nil {
			t.Fatal(err)
		}
		got, err := bulk.ReadFile(ctx, podwire.FileReadHeader{Path: "/tmp/big"})
		if err != nil || !bytes.Equal(got, big) {
			t.Fatalf("read back %d bytes: %v", len(got), err)
		}
		t.Logf("6 MiB over the bulk session and back in %v", time.Since(start))
	})

	if bundle := os.Getenv("FLEET_TEST_TMUX_BUNDLE"); bundle != "" {
		t.Run("bundled tmux", func(t *testing.T) {
			lv.store.Getenv = envOf(map[string]string{binstore.EnvBinaryPrefix + strings.ToUpper(runtime.GOARCH): v1.Path, binstore.EnvTmuxPrefix + strings.ToUpper(runtime.GOARCH): bundle})
			dir, err := s.EnsureTmux(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if out := run(t, s.Client(), dir+"/bin/tmux -V"); !strings.HasPrefix(out, "tmux ") {
				t.Fatalf("tmux -V: %q", out)
			}
			out := run(t, s.Client(), fmt.Sprintf(`export TMUX_TMPDIR=$(mktemp -d); %[1]s/bin/tmux new-session -d -s t 'sleep 60' && %[1]s/bin/tmux list-sessions -F '#S'; %[1]s/bin/tmux kill-server`, dir))
			if !strings.Contains(out, "t\n") {
				t.Fatalf("a session on the bundled tmux: %q", out)
			}
		})
	}

	t.Run("fallbacks", func(t *testing.T) {
		var out bytes.Buffer
		code, err := pool.FallbackExec(ctx, target, execas.Request{Class: execas.ClassExec, Argv: []string{"sh", "-c", "read x; echo got $x as $(id -un)"}}, kubeexec.Stdio{Stdin: strings.NewReader("in\n"), Stdout: &out})
		if err != nil || code != 0 || out.String() != "got in as vscode\n" {
			t.Fatalf("fallback exec: %d %v %q", code, err, out.String())
		}
		out.Reset()
		if code, err := pool.RawScript(ctx, target, "echo raw; exit 5", &out, nil); err != nil || code != 5 || out.String() != "raw\n" {
			t.Fatalf("raw script: %d %v %q", code, err, out.String())
		}
	})

	t.Run("fast path", func(t *testing.T) {
		before := execs.snapshot()
		_ = s.Close()
		waitDone(t, s.Done(), "the closed session")
		s2, err := pool.EnsureAgent(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		after := execs.snapshot()
		if after["probe"] != before["probe"] || after["stage"] != before["stage"] || after["serve"] != before["serve"]+1 {
			t.Fatalf("reconnect execs %v -> %v, want one serve", before, after)
		}
		s = s2
	})

	t.Run("lazy restage", func(t *testing.T) {
		v2 := lv.buildPod("vlive-2")
		upgraded, upExecs := lv.pool(v2)
		s2, err := upgraded.EnsureAgent(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if s2.Hello().Version != "vlive-1" || !s2.Stale() {
			t.Fatalf("the upgraded daemon got %s (stale %v)", s2.Hello().Version, s2.Stale())
		}
		waitDone(t, s.Done(), "the old daemon's session after the takeover")
		restage := target
		restage.Restage = true
		s3, err := upgraded.EnsureAgent(ctx, restage)
		if err != nil {
			t.Fatal(err)
		}
		if s3.Hello().Version != "vlive-2" || s3.Stale() {
			t.Fatalf("after the restage: %s (stale %v)", s3.Hello().Version, s3.Stale())
		}
		if got := upExecs.snapshot(); got["stage"] != 1 || got["serve"] != 2 {
			t.Fatalf("restage execs %v", got)
		}
		if spec := lv.pid1(secret); spec["binary_sha256"] != shaOfFile(t, v2.Path) || spec["fallback_sha256"] != sha1 {
			t.Fatalf("pid1.json after the restage: %v", spec)
		}
		out := run(t, s3.Client(), "ls /opt/fleet/bin | sort | tr '\\n' ' '")
		if !strings.Contains(out, "fleet-"+sha1) || !strings.Contains(out, "fleet-"+shaOfFile(t, v2.Path)) || strings.Count(out, "fleet-") != 2 {
			t.Fatalf("the pod's bin dir: %q", out)
		}
	})

	t.Run("issued transports", func(t *testing.T) {
		lv.checkIssued(ctx, lv.rec, pod.Name, "the context's own")
		lv.checkTokenAndPluginContexts(ctx, pod.Name)
	})

	t.Run("debug container", func(t *testing.T) {
		// `kubectl debug` adds an ephemeral container to the live pod: the
		// pod is no longer the one fleet rendered.
		live, err := lv.cs.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		live.Spec.EphemeralContainers = append(live.Spec.EphemeralContainers, corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "debugger", Image: image, Command: []string{"sleep", "infinity"}, ImagePullPolicy: corev1.PullIfNotPresent,
		}})
		if _, err := lv.cs.CoreV1().Pods(ns).UpdateEphemeralContainers(ctx, pod.Name, live, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		fresh, _ := lv.pool(lv.buildPod("vlive-2"))
		var tampered *TamperedError
		if _, err := fresh.EnsureAgent(ctx, target); !errors.As(err, &tampered) || !strings.Contains(err.Error(), "spec differs") {
			t.Fatalf("a pod with a debug container: %v, want kubernetes:tampered", err)
		}
	})

	t.Run("substituted pod", func(t *testing.T) {
		before := execs.snapshot()
		zero := int32(0)
		set, _ := lv.cs.AppsV1().StatefulSets(ns).Get(ctx, sts.Name, metav1.GetOptions{})
		set.Spec.Replicas = &zero
		if _, err := lv.cs.AppsV1().StatefulSets(ns).Update(ctx, set, metav1.UpdateOptions{FieldManager: DefaultFieldManager}); err != nil {
			t.Fatal(err)
		}
		lv.waitGone(pod.Name)
		impostor := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: ns}, Spec: *sts.Spec.Template.Spec.DeepCopy()}
		if _, err := lv.cs.CoreV1().Pods(ns).Create(ctx, impostor, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		lv.waitRunning(sts)
		var tampered *TamperedError
		if _, err := pool.EnsureAgent(ctx, target); !errors.As(err, &tampered) {
			t.Fatalf("a substituted pod: %v, want kubernetes:tampered", err)
		} else {
			t.Logf("refused: %v", err)
		}
		if _, err := pool.FallbackExec(ctx, target, execas.Request{Class: execas.ClassRoot, Argv: []string{"true"}}, kubeexec.Stdio{}); !errors.As(err, &tampered) {
			t.Fatalf("fallback into a substituted pod: %v", err)
		}
		if got := execs.snapshot(); fmt.Sprint(got) != fmt.Sprint(before) {
			t.Fatalf("execs into the substituted pod: %v -> %v", before, got)
		}
	})
}

type live struct {
	t     *testing.T
	ns    string
	rec   fleet.KubeCluster
	conn  *kubeexec.Connection
	cs    kclient.Interface
	store *binstore.Store
	dir   string
}

func newLive(t *testing.T, kctx string) *live {
	t.Helper()
	paths := kubeexec.DefaultKubeconfigPaths(os.Environ())
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	lv := &live{t: t, ns: "fleet-it-p2-10a-pool-" + hex.EncodeToString(suffix[:]), dir: t.TempDir()}
	lv.rec = fleet.KubeCluster{ID: "live-" + hex.EncodeToString(suffix[:]), Context: kctx, KubeconfigPaths: paths, Namespace: lv.ns}
	conn, err := kubeexec.Load(lv.rec, kubeexec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lv.conn = conn
	if lv.cs, err = conn.Clientset(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := lv.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lv.ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lv.cs.CoreV1().Namespaces().Delete(context.Background(), lv.ns, metav1.DeleteOptions{}) })
	return lv
}

// buildPod builds the real pod build of fleet with version stamped and makes
// the store serve it as the daemon's own version.
func (lv *live) buildPod(version string) *binstore.Artifact {
	lv.t.Helper()
	out := filepath.Join(lv.dir, version, binstore.BinaryAsset(runtime.GOARCH))
	cmd := exec.Command("go", "build", "-tags", "nok8s", "-ldflags", "-s -w -X "+binstore.VersionSymbol+"="+version, "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if msg, err := cmd.CombinedOutput(); err != nil {
		lv.t.Fatalf("build the pod binary: %v\n%s", err, msg)
	}
	lv.store = &binstore.Store{Dir: filepath.Dir(out), Self: binstore.Identity{Version: version}, Getenv: envOf(nil)}
	a, err := lv.store.Binary(context.Background(), runtime.GOARCH)
	if err != nil {
		lv.t.Fatal(err)
	}
	return a
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type execCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *execCounter) add(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[kind]++
}

func (c *execCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

// pool is a pool over the live connection whose store serves art.
func (lv *live) pool(art *binstore.Artifact) (*Pool, *execCounter) {
	counter := &execCounter{}
	store := &binstore.Store{Dir: filepath.Dir(art.Path), Self: art.Info.Identity, Getenv: envOf(nil)}
	lv.store = store
	p := NewPool(PoolOptions{
		Records: func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{lv.rec}, nil },
		Bins:    store,
		Version: art.Info.Version,
		Logger:  slog.New(slog.NewTextHandler(testLog{lv.t}, nil)),
		Exec: func(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
			counter.add(execKind(req.Command))
			return c.Conn.Exec(ctx, req, stdio)
		},
	})
	lv.t.Cleanup(p.Close)
	return p, counter
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// applyInstance creates an instance-shaped StatefulSet (fleet's field
// manager, the pod's dirs as emptyDirs, the spec Secret at /opt/fleet/spec)
// and returns it as the API server stored it, with its spec Secret's name
// and the spec hash of the pod as rendered (what fleetd journals).
func (lv *live) applyInstance(image string) (*appsv1.StatefulSet, string, string) {
	lv.t.Helper()
	ctx := context.Background()
	name := "fi-live-one-k8sl1ve0"
	secret := name + "-spec-g1"
	execSpec, _ := json.Marshal(execas.Spec{RemoteUser: "vscode", WorkspaceFolder: "/home/vscode", WorkingDir: "/"})
	pid1, _ := json.Marshal(map[string]any{"format": 1, "argv": []string{"sleep", "infinity"}})
	if _, err := lv.cs.CoreV1().Secrets(lv.ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secret},
		Data:       map[string][]byte{"exec.json": execSpec, podspec.PID1File: pid1},
	}, metav1.CreateOptions{FieldManager: DefaultFieldManager}); err != nil {
		lv.t.Fatal(err)
	}
	one, no, mode := int32(1), false, int32(0o400)
	labels := map[string]string{"app.kubernetes.io/managed-by": "fleet", "fleet.dev/key": "k8sl1ve0"}
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one, ServiceName: name,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  &no,
					EnableServiceLinks:            &no,
					TerminationGracePeriodSeconds: ptrInt64(1),
					Containers: []corev1.Container{{
						Name: DevContainer, Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{"sleep", "infinity"},
						Env:     []corev1.EnvVar{{Name: "FLEET_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "fleet", MountPath: "/opt/fleet"},
							{Name: "spec", MountPath: "/opt/fleet/spec", ReadOnly: true},
							{Name: "run", MountPath: "/opt/fleet/run"},
							{Name: "control", MountPath: "/fleet-mounts/control"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "fleet", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "spec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret, DefaultMode: &mode}}},
						{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "control", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	rendered := podspec.SpecHash(&set.Spec.Template.Spec)
	got, err := lv.cs.AppsV1().StatefulSets(lv.ns).Create(ctx, set, metav1.CreateOptions{FieldManager: DefaultFieldManager})
	if err != nil {
		lv.t.Fatal(err)
	}
	return got, secret, rendered
}

func ptrInt64(v int64) *int64 { return &v }

func (lv *live) waitRunning(sts *appsv1.StatefulSet) *corev1.Pod {
	lv.t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		p, err := lv.cs.CoreV1().Pods(lv.ns).Get(context.Background(), sts.Name+"-0", metav1.GetOptions{})
		if err == nil && p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			if st := containerStatus(p, DevContainer); st != nil && st.State.Running != nil && st.ImageID != "" {
				return p
			}
		}
		if time.Now().After(deadline) {
			lv.t.Fatalf("pod %s-0 is not running: %v %+v", sts.Name, err, p)
		}
		time.Sleep(time.Second)
	}
}

func (lv *live) waitGone(pod string) {
	lv.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := lv.cs.CoreV1().Pods(lv.ns).Get(context.Background(), pod, metav1.GetOptions{}); err != nil {
			return
		}
		if time.Now().After(deadline) {
			lv.t.Fatalf("pod %s is still there", pod)
		}
		time.Sleep(time.Second)
	}
}

func (lv *live) pid1(secret string) map[string]any {
	lv.t.Helper()
	sec, err := lv.cs.CoreV1().Secrets(lv.ns).Get(context.Background(), secret, metav1.GetOptions{})
	if err != nil {
		lv.t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(sec.Data[podspec.PID1File], &m)
	return m
}

// checkIssued issues rec's transport through a pool and, as the kube-exec
// shim's direct mode would, dials the pod with it over WebSocket and SPDY.
func (lv *live) checkIssued(ctx context.Context, rec fleet.KubeCluster, pod, what string) fleet.KubeExecCredential {
	lv.t.Helper()
	p := NewPool(PoolOptions{Records: func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil }})
	defer p.Close()
	cred, err := p.IssueExecCredential(ctx, rec.ID, false)
	if err != nil {
		lv.t.Fatalf("%s: issue: %v", what, err)
	}
	cfg, err := kubeexec.RESTConfigFor(cred)
	if err != nil {
		lv.t.Fatal(err)
	}
	for _, proto := range []string{kubeexec.ProtocolWebSocket, kubeexec.ProtocolSPDY} {
		var out bytes.Buffer
		req := kubeexec.ExecRequest{Namespace: lv.ns, Pod: pod, Container: DevContainer, Command: []string{"sh", "-c", "cat; echo direct"}, Stdin: true, Protocol: proto}
		if err := kubeexec.ExecConfig(ctx, cfg, req, kubeexec.Stdio{Stdin: strings.NewReader("in-"), Stdout: &out, Stderr: io.Discard}); err != nil || out.String() != "in-direct\n" {
			lv.t.Fatalf("%s (%s auth) over %s: %v %q", what, cred.Auth, proto, err, out.String())
		}
	}
	var exit *kubeexec.ExitError
	if err := kubeexec.ExecConfig(ctx, cfg, kubeexec.ExecRequest{Namespace: lv.ns, Pod: pod, Container: DevContainer, Command: []string{"sh", "-c", "exit 7"}}, kubeexec.Stdio{Stdout: io.Discard, Stderr: io.Discard}); !errors.As(err, &exit) || exit.Code != 7 {
		lv.t.Fatalf("%s: exit code: %v", what, err)
	}
	lv.t.Logf("%s: %s credential (expires %v) dials the pod", what, cred.Auth, cred.ExpiresAt)
	return cred
}

// checkTokenAndPluginContexts issues a ServiceAccount token context, an
// exec-plugin context minting that token, and the token context through a
// kubeconfig proxy-url, and dials the pod with each.
func (lv *live) checkTokenAndPluginContexts(ctx context.Context, pod string) {
	lv.t.Helper()
	cs := lv.cs
	sa := "fleet-live-shim"
	if _, err := cs.CoreV1().ServiceAccounts(lv.ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa}}, metav1.CreateOptions{}); err != nil {
		lv.t.Fatal(err)
	}
	if _, err := cs.RbacV1().Roles(lv.ns).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: sa}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create", "get"}},
	}}, metav1.CreateOptions{}); err != nil {
		lv.t.Fatal(err)
	}
	if _, err := cs.RbacV1().RoleBindings(lv.ns).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: sa},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: sa},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: lv.ns}},
	}, metav1.CreateOptions{}); err != nil {
		lv.t.Fatal(err)
	}
	expiry := int64(3600)
	tr, err := cs.CoreV1().ServiceAccounts(lv.ns).CreateToken(ctx, sa, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry}}, metav1.CreateOptions{})
	if err != nil {
		lv.t.Fatal(err)
	}
	base := lv.conn.RESTConfig()
	ca := base.CAData
	if len(ca) == 0 && base.CAFile != "" {
		ca, _ = os.ReadFile(base.CAFile)
	}
	write := func(name, cluster, user string) fleet.KubeCluster {
		path := filepath.Join(lv.dir, name+".yaml")
		cfg := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: %s\n    certificate-authority-data: %s\n%scontexts:\n- name: ctx\n  context: {cluster: c, user: u, namespace: %s}\nusers:\n- name: u\n  user:\n%s",
			base.Host, b64(ca), cluster, lv.ns, user)
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			lv.t.Fatal(err)
		}
		return fleet.KubeCluster{ID: "live-" + name, Context: "ctx", KubeconfigPaths: []string{path}, Namespace: lv.ns, ExtraPath: []string{lv.dir}}
	}

	tokenRec := write("token", "", "    token: "+tr.Status.Token+"\n")
	if cred := lv.checkIssued(ctx, tokenRec, pod, "ServiceAccount token"); cred.Auth != string(kubeexec.AuthToken) || cred.ExpiresAt.IsZero() {
		lv.t.Fatalf("token credential %s expiring %v", cred.Auth, cred.ExpiresAt)
	}

	counter := filepath.Join(lv.dir, "plugin-runs")
	out := fmt.Sprintf(`{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":%q,"expirationTimestamp":%q}}`,
		tr.Status.Token, tr.Status.ExpirationTimestamp.UTC().Format(time.RFC3339))
	_ = os.WriteFile(filepath.Join(lv.dir, "plugin-out.json"), []byte(out), 0o600)
	_ = os.WriteFile(filepath.Join(lv.dir, "fleet-live-plugin"), []byte(fmt.Sprintf("#!/bin/sh\necho run >> %q\ncat %q\n", counter, filepath.Join(lv.dir, "plugin-out.json"))), 0o755)
	pluginRec := write("plugin", "", "    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: fleet-live-plugin\n      interactiveMode: Never\n")
	p := NewPool(PoolOptions{Records: func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{pluginRec}, nil }})
	defer p.Close()
	for range 3 {
		cred, err := p.IssueExecCredential(ctx, pluginRec.ID, false)
		if err != nil || cred.BearerToken != tr.Status.Token || cred.Auth != string(kubeexec.AuthExecPlugin) {
			lv.t.Fatalf("plugin issue: %s %v", cred.Auth, err)
		}
	}
	if runs, _ := os.ReadFile(counter); strings.Count(string(runs), "run") != 1 {
		lv.t.Fatalf("3 issues ran the plugin %d times, want once", strings.Count(string(runs), "run"))
	}
	lv.checkIssued(ctx, pluginRec, pod, "exec plugin")

	proxy, tunnels := liveProxy(lv.t)
	proxyRec := write("proxy", "    proxy-url: "+proxy+"\n", "    token: "+tr.Status.Token+"\n")
	if cred := lv.checkIssued(ctx, proxyRec, pod, "proxy-url"); cred.ProxyURL != proxy {
		lv.t.Fatalf("issued proxy %q, want %q", cred.ProxyURL, proxy)
	}
	if tunnels.Load() == 0 {
		lv.t.Fatal("the proxy-url context did not go through its proxy")
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// liveProxy is a CONNECT proxy for the API server.
func liveProxy(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var tunnels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		tunnels.Add(1)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = dst.Close()
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(dst, rw); _ = dst.Close() }()
		go func() { _, _ = io.Copy(conn, dst); _ = conn.Close() }()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &tunnels
}
