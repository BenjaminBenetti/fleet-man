//go:build !nok8s && linux

package kubernetes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// The tests below run the whole bootstrap against local "pods": each pod is
// a directory tree, and an exec into it runs the command on this host with
// the pod's directories in FAKEPOD_ROOT (testdata/fakepod, the pod build of
// the real agent with configurable paths). The cluster's objects come from a
// fake clientset. Only the API server's exec transport is not exercised here
// (see the live test).

var (
	fakePodOnce sync.Once
	fakePodDir  string
	fakePodErr  error
)

// fakePod is the fake pod binary of version v (vtest-1 or vtest-2), built
// as the pod build (-tags nok8s) with the version stamped as releases are.
func fakePod(t *testing.T, v string) string {
	t.Helper()
	fakePodOnce.Do(func() {
		fakePodDir, fakePodErr = os.MkdirTemp("", "fakepod-")
		if fakePodErr != nil {
			return
		}
		for _, version := range []string{"vtest-1", "vtest-2"} {
			cmd := exec.Command("go", "build", "-tags", "nok8s",
				"-ldflags", "-X "+binstore.VersionSymbol+"="+version,
				"-o", filepath.Join(fakePodDir, version), "./testdata/fakepod")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				fakePodErr = fmt.Errorf("build fakepod: %v\n%s", err, out)
				return
			}
		}
	})
	if fakePodErr != nil {
		t.Fatal(fakePodErr)
	}
	return filepath.Join(fakePodDir, v)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakePodDir != "" {
		_ = os.RemoveAll(fakePodDir)
	}
	os.Exit(code)
}

type execRecord struct {
	pod, kind string
}

type harness struct {
	t      *testing.T
	ns     string
	cs     *fake.Clientset
	store  *binstore.Store
	env    map[string]string
	envMu  sync.Mutex
	roots  map[string]string // pod -> its root
	pool   *Pool
	rec    fleet.KubeCluster
	now    atomic.Pointer[time.Time]
	failMu sync.Mutex
	fail   map[string]error // pod -> what its execs fail with
	// literal: the pool's layout already names the pod's local root, so
	// exec argvs are run as they are.
	literal bool

	execMu sync.Mutex
	execs  []execRecord
	// onExec runs at the start of every exec.
	onExec func(execRecord)

	// deaf: what fleetd sends the pods from now on is held back until the
	// test ends (an agent that no longer answers).
	deaf     atomic.Bool
	unheard  chan struct{}
	hearOnce sync.Once
}

const (
	testCluster = "test-cluster-a1b2c3"
	testOwner   = "owner1234567"
)

func newHarness(t *testing.T, opts PoolOptions) *harness {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the fake pod binary")
	}
	h := &harness{t: t, ns: "fleet-test", cs: fake.NewClientset(), env: map[string]string{}, roots: map[string]string{}, fail: map[string]error{}, unheard: make(chan struct{})}
	t.Cleanup(func() { h.hearOnce.Do(func() { close(h.unheard) }) })
	now := time.Now()
	h.now.Store(&now)
	h.env[binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH)] = fakePod(t, "vtest-1")
	h.store = &binstore.Store{Dir: t.TempDir(), Self: binstore.Identity{Version: "vtest-1"}, Getenv: func(k string) string {
		h.envMu.Lock()
		defer h.envMu.Unlock()
		return h.env[k]
	}}

	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "config")
	writeKubeconfig(t, kubeconfig, "tok-one")
	h.rec = fleet.KubeCluster{ID: testCluster, Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: h.ns}

	opts.Records = func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{h.rec}, nil }
	opts.Environ = func() []string { return []string{"PATH=/usr/bin:/bin"} }
	opts.Bins = h.store
	opts.SpecHash = testHash
	opts.Exec = h.exec
	opts.Clientset = func(*Cluster) (kclient.Interface, error) { return h.cs, nil }
	opts.Version = "vtest-1"
	if opts.Owner == nil {
		opts.Owner = func() (string, error) { return testOwner, nil }
	}
	opts.Now = func() time.Time {
		if now := h.now.Load(); now != nil {
			return *now
		}
		return time.Now()
	}
	if opts.Layout == (PodLayout{}) {
		opts.Layout = PodLayout{InstallDir: "/opt/fleet", BinDir: "/opt/fleet/bin"}
	}
	h.pool = NewPool(opts)
	t.Cleanup(h.pool.Close)
	return h
}

func writeKubeconfig(t *testing.T, path, token string) {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: ctx
  context: {cluster: c, user: u}
users:
- name: u
  user: {token: %s}
`, token)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// addPod creates an instance's StatefulSet, its running pod and its spec
// Secret, and a local root for the pod, and returns the target.
func (h *harness) addPod(name string) PodTarget {
	h.t.Helper()
	return h.addPodAs(name, "fi-fleet-"+name+"-k8sk8sk8")
}

// addPodAs is addPod with the StatefulSet named sts.
func (h *harness) addPodAs(name, sts string) PodTarget {
	h.t.Helper()
	set, pod := instanceObjects(h.ns, sts, types.UID("uid-"+name))
	// The instance labels the pool's informers select by.
	set.Labels, pod.Labels = instanceLabels(), instanceLabels()
	ctx := context.Background()
	// The tracker keeps the managedFields as given (a Create through the
	// fake drops them).
	if err := h.cs.Tracker().Add(set); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.cs.CoreV1().Pods(h.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	pid1, _ := json.Marshal(map[string]any{"format": 1, "argv": []string{"sleep", "infinity"}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sts + "-spec-g1", Namespace: h.ns}, Data: map[string][]byte{podspec.PID1File: pid1}}
	if _, err := h.cs.CoreV1().Secrets(h.ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "kp")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		// The pod's agents end with the pool; whatever they still write as
		// they exit goes with a second sweep.
		h.pool.Close()
		_ = os.RemoveAll(root)
		time.Sleep(200 * time.Millisecond)
		_ = os.RemoveAll(root)
	})
	spec, _ := json.Marshal(execas.Spec{UserEnvProbe: execas.ProbeNone, WorkingDir: "/"})
	_ = os.WriteFile(filepath.Join(root, "exec.json"), spec, 0o600)
	h.roots[pod.Name] = root
	return PodTarget{Cluster: testCluster, Fleet: "fleet", Instance: name, StatefulSet: sts, Expect: expectFor(set, pod), SpecSecret: secret.Name, Priority: PriorityInteractive}
}

// instanceLabels are an instance object's labels on the harness's
// connection (names.Base plus role=instance).
func instanceLabels() map[string]string {
	l := names.Base(testOwner, testCluster)
	l[names.LabelRole] = string(names.RoleInstance)
	return l
}

// rootPath maps a pod path to the pod's local root.
func (h *harness) rootPath(pod, p string) string { return filepath.Join(h.roots[pod], p) }

// exec runs an exec in a local pod: the pod's paths (/opt/fleet/...) are
// rewritten into its root, as its filesystem would hold them.
func (h *harness) exec(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
	root, ok := h.roots[req.Pod]
	if !ok || req.Namespace != h.ns || req.Container != DevContainer {
		return fmt.Errorf("no pod %s/%s container %q", req.Namespace, req.Pod, req.Container)
	}
	rec := execRecord{pod: req.Pod, kind: execKind(req.Command)}
	h.execMu.Lock()
	h.execs = append(h.execs, rec)
	hook := h.onExec
	h.execMu.Unlock()
	if hook != nil {
		hook(rec)
	}
	h.failMu.Lock()
	failure := h.fail[req.Pod]
	h.failMu.Unlock()
	if failure != nil {
		return failure
	}
	argv := make([]string, len(req.Command))
	for i, a := range req.Command {
		if h.literal {
			argv[i] = a
		} else {
			argv[i] = strings.ReplaceAll(a, "/opt/fleet", root+"/opt/fleet")
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// The tracker, not the clientset: the harness's own read is no API
	// call of the pool's.
	uid := ""
	if obj, err := h.cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), h.ns, req.Pod); err == nil {
		if pod, ok := obj.(*corev1.Pod); ok {
			uid = string(pod.UID)
		}
	}
	cmd.Env = append(os.Environ(), "FAKEPOD_ROOT="+root, "FLEET_POD_UID="+uid)
	cmd.Stdout, cmd.Stderr = stdio.Stdout, stdio.Stderr
	cmd.WaitDelay = time.Second
	// Like a kube exec, the exec ends when the command does, without
	// waiting for the end of the caller's stdin.
	var stdin io.WriteCloser
	if req.Stdin && stdio.Stdin != nil {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(stdin, deafReader{h, stdio.Stdin})
			_ = stdin.Close()
		}()
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return &kubeexec.ExitError{Code: exitErr.ExitCode()}
	}
	return err
}

// deafReader is a pod's stdin, held back while the harness is deaf.
type deafReader struct {
	h *harness
	r io.Reader
}

func (d deafReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if d.h.deaf.Load() {
		<-d.h.unheard
	}
	return n, err
}

func execKind(argv []string) string {
	switch {
	case len(argv) == 3 && argv[0] == "sh" && strings.Contains(argv[2], "self-version="):
		return "probe"
	case len(argv) == 3 && argv[0] == "sh" && strings.Contains(argv[2], ".fleet.new"):
		return "stage"
	case len(argv) >= 3 && argv[1] == "pod-agent" && argv[2] == "serve":
		if strings.Contains(strings.Join(argv, " "), "--role bulk") {
			return "serve-bulk"
		}
		return "serve"
	case len(argv) == 2 && argv[1] == "exec-as":
		return "exec-as"
	case len(argv) == 2 && argv[0] == "sh" && argv[1] == "-s":
		return "raw"
	}
	return strings.Join(argv, " ")
}

// counts is how many execs of each kind ran, for pod ("" for every pod).
func (h *harness) counts(pod string) map[string]int {
	h.execMu.Lock()
	defer h.execMu.Unlock()
	out := map[string]int{}
	for _, e := range h.execs {
		if pod == "" || e.pod == pod {
			out[e.kind]++
		}
	}
	return out
}

func (h *harness) wantCounts(pod string, want map[string]int) {
	h.t.Helper()
	got := h.counts(pod)
	for k, v := range want {
		if got[k] != v {
			h.t.Fatalf("execs into %s: %v, want %v", pod, got, want)
		}
	}
	for k, v := range got {
		if want[k] != v {
			h.t.Fatalf("execs into %s: %v, want %v", pod, got, want)
		}
	}
}

func (h *harness) pid1(target PodTarget) map[string]any {
	h.t.Helper()
	sec, err := h.cs.CoreV1().Secrets(h.ns).Get(context.Background(), target.SpecSecret, metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(sec.Data[podspec.PID1File], &m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

func run(t *testing.T, c *podwire.Client, script string) string {
	t.Helper()
	res, err := c.Run(context.Background(), podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", script}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 0 {
		t.Fatalf("%q exited %d: %s", script, res.Code, res.Stderr)
	}
	return string(res.Stdout)
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not end", what)
	}
}

func TestPoolBootstrapStagesThenServes(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	pod := target.Pod()
	ctx := context.Background()

	s, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 1})
	if hello := s.Hello(); hello.Version != "vtest-1" || hello.PodUID != "uid-one" || s.Stale() || s.Pod().UID != "uid-one" || s.Arch() != runtime.GOARCH {
		t.Fatalf("session %+v, stale %v", hello, s.Stale())
	}
	if out := run(t, s.Client(), "echo hi"); out != "hi\n" {
		t.Fatalf("ran %q", out)
	}
	sha := shaOfFile(t, fakePod(t, "vtest-1"))
	if target, _ := os.Readlink(h.rootPath(pod, "opt/fleet/bin/fleet")); target != "fleet-"+sha {
		t.Fatalf("the pod's fleet link -> %q, want fleet-%s", target, sha)
	}
	if spec := h.pid1(target); spec["binary_sha256"] != sha || spec["fallback_sha256"] != nil || spec["format"] != float64(1) {
		t.Fatalf("pid1.json after the first stage: %v", spec)
	}

	// The live session is reused: no exec at all.
	again, err := h.pool.EnsureAgent(ctx, target)
	if err != nil || again != s {
		t.Fatalf("second ensure: %p %v, want the same session %p", again, err, s)
	}
	if h.pool.Session(target) != s {
		t.Fatal("Session does not report the live session")
	}
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 1})

	// After the session drops, the same pod reconnects on the fast path:
	// no probe, no stage, one exec.
	_ = s.Close()
	waitDone(t, s.Done(), "the closed session")
	s2, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 2})
	if out := run(t, s2.Client(), "echo again"); out != "again\n" {
		t.Fatalf("ran %q", out)
	}

	// A new pod (another UID) is probed again.
	_ = s2.Close()
	waitDone(t, s2.Done(), "the second session")
	p, _ := h.cs.CoreV1().Pods(h.ns).Get(ctx, pod, metav1.GetOptions{})
	p.UID = "uid-one-recreated"
	_, _ = h.cs.CoreV1().Pods(h.ns).Update(ctx, p, metav1.UpdateOptions{})
	if _, err := h.pool.EnsureAgent(ctx, target); err != nil {
		t.Fatal(err)
	}
	h.wantCounts(pod, map[string]int{"probe": 2, "stage": 1, "serve": 3})
}

func shaOfFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return hexSHA(data)
}

// A daemon of a newer build accepts an older agent that speaks its protocol
// and restages it only when asked (the lazy moments); the restage takes the
// agent role over and points pid1.json at the new binary, the old one its
// fallback.
func TestPoolRestagesLazily(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	pod := target.Pod()
	ctx := context.Background()
	old, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	v1, v2 := shaOfFile(t, fakePod(t, "vtest-1")), shaOfFile(t, fakePod(t, "vtest-2"))

	// The daemon is upgraded: a new process, a new pool, the v2 build.
	h.envMu.Lock()
	h.env[binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH)] = fakePod(t, "vtest-2")
	h.envMu.Unlock()
	h.store = &binstore.Store{Dir: t.TempDir(), Self: binstore.Identity{Version: "vtest-2"}, Getenv: h.store.Getenv}
	opts := h.pool.opts
	opts.Bins, opts.Version = h.store, "vtest-2"
	upgraded := NewPool(opts)
	t.Cleanup(upgraded.Close)

	s, err := upgraded.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if s.Hello().Version != "vtest-1" || !s.Stale() {
		t.Fatalf("an upgraded daemon got agent %s, stale %v; want the old agent, accepted", s.Hello().Version, s.Stale())
	}
	waitDone(t, old.Done(), "the old daemon's session, after the takeover")
	h.wantCounts(pod, map[string]int{"probe": 2, "stage": 1, "serve": 2})

	restage := target
	restage.Restage = true
	s2, err := upgraded.EnsureAgent(ctx, restage)
	if err != nil {
		t.Fatal(err)
	}
	if s2 == s || s2.Hello().Version != "vtest-2" || s2.Stale() || s2.Hello().SHA != v2 {
		t.Fatalf("after the restage: %+v stale %v", s2.Hello(), s2.Stale())
	}
	waitDone(t, s.Done(), "the stale session")
	h.wantCounts(pod, map[string]int{"probe": 2, "stage": 2, "serve": 3})
	if spec := h.pid1(target); spec["binary_sha256"] != v2 || spec["fallback_sha256"] != v1 {
		t.Fatalf("pid1.json after the restage: %v", spec)
	}
	entries, _ := os.ReadDir(h.rootPath(pod, "opt/fleet/bin"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 3 || !strings.Contains(strings.Join(names, " "), v1) || !strings.Contains(strings.Join(names, " "), v2) {
		t.Fatalf("the pod's bin dir holds %v, want the link, v2 and v1 (pod-init's fallback)", names)
	}
	// Restaging an up to date pod does nothing.
	if s3, err := upgraded.EnsureAgent(ctx, restage); err != nil || s3 != s2 {
		t.Fatalf("restage of a current agent: %v", err)
	}
}

func TestPoolRefusesATamperedPod(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	ctx := context.Background()
	p, _ := h.cs.CoreV1().Pods(h.ns).Get(ctx, target.Pod(), metav1.GetOptions{})
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
	_, _ = h.cs.CoreV1().Pods(h.ns).Update(ctx, p, metav1.UpdateOptions{})

	var tampered *TamperedError
	if _, err := h.pool.EnsureAgent(ctx, target); !errors.As(err, &tampered) {
		t.Fatalf("ensure: %v, want kubernetes:tampered", err)
	}
	if _, err := h.pool.FallbackExec(ctx, target, execas.Request{Class: execas.ClassScript, Argv: []string{"true"}}, kubeexec.Stdio{}); !errors.As(err, &tampered) {
		t.Fatalf("fallback: %v, want kubernetes:tampered", err)
	}
	if _, err := h.pool.RawScript(ctx, target, "true", nil, nil); !errors.As(err, &tampered) {
		t.Fatalf("raw: %v, want kubernetes:tampered", err)
	}
	h.wantCounts(target.Pod(), map[string]int{})
}

func TestPoolBulkSessionOpensOnDemandAndClosesIdle(t *testing.T) {
	h := newHarness(t, PoolOptions{BulkIdle: 300 * time.Millisecond})
	target := h.addPod("one")
	ctx := context.Background()
	s, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	bulk, err := s.Bulk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bulk.Hello().Role != podwire.RoleBulk {
		t.Fatalf("bulk role %q", bulk.Hello().Role)
	}
	if again, _ := s.Bulk(ctx); again != bulk {
		t.Fatal("a second Bulk opened another session")
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 3<<16) // 3 MiB, over the agent session's cap
	path := h.rootPath(target.Pod(), "big")
	if err := bulk.WriteFile(ctx, podwire.FileWriteHeader{Path: path, Mode: 0o600, Size: int64(len(big)), SHA256: hexSHA(big)}, bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, big) {
		t.Fatal("the bulk session wrote something else")
	}
	h.now.Store(ptrTime(time.Now().Add(time.Hour))) // the clock the idle check reads
	waitDone(t, bulk.Done(), "the idle bulk session")
	h.wantCounts(target.Pod(), map[string]int{"probe": 1, "stage": 1, "serve": 1, "serve-bulk": 1})
	if !errors.Is(s.Err(), nil) {
		t.Fatalf("closing the bulk session ended the agent session: %v", s.Err())
	}
	if _, err := s.Bulk(ctx); err != nil {
		t.Fatal(err)
	}
	h.wantCounts(target.Pod(), map[string]int{"probe": 1, "stage": 1, "serve": 1, "serve-bulk": 2})

	// Closing the agent session closes the bulk one with it.
	b2, _ := s.Bulk(ctx)
	_ = s.Close()
	waitDone(t, b2.Done(), "the bulk session of a closed session")
	if _, err := s.Bulk(ctx); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("bulk of a closed session: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestPoolInstallsTheBundledTmux(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	// The agent runs the bundle's install itself, at the layout's paths:
	// point the layout at the pod's root.
	h.literal = true
	h.pool.opts.Layout = PodLayout{InstallDir: h.rootPath(target.Pod(), "opt/fleet"), BinDir: h.rootPath(target.Pod(), "opt/fleet/bin")}
	// A bundle shaped like the real one; its "tmux" is an ELF of this arch.
	bundle := filepath.Join(t.TempDir(), "tmux.tar.gz")
	writeTmuxBundle(t, bundle, fakePod(t, "vtest-1"))
	h.envMu.Lock()
	h.env[binstore.EnvTmuxPrefix+strings.ToUpper(runtime.GOARCH)] = bundle
	h.envMu.Unlock()
	ctx := context.Background()
	s, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := s.EnsureTmux(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dir != h.rootPath(target.Pod(), "opt/fleet/tmux") {
		t.Fatalf("tmux dir %q", dir)
	}
	local := h.rootPath(target.Pod(), "opt/fleet/tmux/bin/tmux")
	if fi, err := os.Stat(local); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bin/tmux in the pod: %v %v", fi, err)
	}
	if link, _ := os.Readlink(h.rootPath(target.Pod(), "opt/fleet/tmux")); link != "tmux-"+shaOfFile(t, bundle) {
		t.Fatalf("tmux link -> %q", link)
	}
	if _, err := s.EnsureTmux(ctx); err != nil {
		t.Fatal(err)
	}
	// A new session finds it installed and streams nothing.
	_ = s.Close()
	waitDone(t, s.Done(), "the session")
	s2, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.EnsureTmux(ctx); err != nil {
		t.Fatal(err)
	}
	h.wantCounts(target.Pod(), map[string]int{"probe": 1, "stage": 1, "serve": 2, "serve-bulk": 1})
}

func writeTmuxBundle(t *testing.T, path, elfBinary string) {
	t.Helper()
	data, _ := os.ReadFile(elfBinary)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range []struct {
		h    tar.Header
		body []byte
	}{
		{tar.Header{Name: "bin/tmux", Typeflag: tar.TypeReg, Mode: 0o755}, data},
		{tar.Header{Name: "share/terminfo/t/tmux-256color", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("terminfo")},
		{tar.Header{Name: "share/terminfo/t/tmux", Typeflag: tar.TypeLink, Linkname: "share/terminfo/t/tmux-256color"}, nil},
	} {
		e.h.Size = int64(len(e.body))
		_ = tw.WriteHeader(&e.h)
		_, _ = tw.Write(e.body)
	}
	_ = tw.Close()
	_ = zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPoolFallbacks(t *testing.T) {
	h := newHarness(t, PoolOptions{FallbackRate: 1000, FallbackBurst: 1000})
	target := h.addPod("one")
	ctx := context.Background()
	s, err := h.pool.EnsureAgent(ctx, target) // stages the binary exec-as runs from
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	var out bytes.Buffer
	code, err := h.pool.FallbackExec(ctx, target, execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", "cat; echo done; exit 3"}},
		kubeexec.Stdio{Stdin: strings.NewReader("in-"), Stdout: &out})
	if err != nil || code != 3 || out.String() != "in-done\n" {
		t.Fatalf("fallback exec: code %d %v, output %q", code, err, out.String())
	}
	out.Reset()
	if code, err := h.pool.FallbackExec(ctx, target, execas.Request{Class: execas.ClassScript, Argv: []string{"no-such-command"}}, kubeexec.Stdio{Stderr: &out}); err != nil || code != 127 {
		t.Fatalf("a missing command: code %d %v (%s)", code, err, out.String())
	}
	if _, err := h.pool.FallbackExec(ctx, target, execas.Request{Class: execas.ClassScript, Argv: []string{"sh"}, TTY: true}, kubeexec.Stdio{}); err == nil {
		t.Fatal("a TTY fallback")
	}
	out.Reset()
	if code, err := h.pool.RawScript(ctx, target, "echo raw; exit 4", &out, nil); err != nil || code != 4 || out.String() != "raw\n" {
		t.Fatalf("raw script: code %d %v, output %q", code, err, out.String())
	}
	got := h.counts(target.Pod())
	if got["exec-as"] != 2 || got["raw"] != 1 {
		t.Fatalf("execs %v", got)
	}
	// The request travels on stdin: argv names only exec-as.
	h.execMu.Lock()
	defer h.execMu.Unlock()
	for _, e := range h.execs {
		if strings.Contains(e.kind, "no-such-command") || strings.Contains(e.kind, "cat;") {
			t.Fatalf("a fallback's command reached argv: %q", e.kind)
		}
	}
}

func TestPoolPacesFallbacks(t *testing.T) {
	h := newHarness(t, PoolOptions{FallbackRate: 5, FallbackBurst: 1})
	target := h.addPod("one")
	ctx := context.Background()
	h.now.Store(nil) // the bucket refills with real time
	start := time.Now()
	for range 3 {
		if _, err := h.pool.RawScript(ctx, target, "true", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("3 fallback execs at 5/s with a burst of 1 took %v", d)
	}
}

func TestPoolLimitsConcurrentBootstraps(t *testing.T) {
	h := newHarness(t, PoolOptions{MaxBootstraps: 2})
	var targets []PodTarget
	for i := range 6 {
		targets = append(targets, h.addPod(fmt.Sprintf("p%d", i)))
	}
	var peak atomic.Int32
	h.onExec = func(execRecord) {
		c, err := h.pool.Cluster(testCluster)
		if err != nil {
			return
		}
		n := int32(c.gates.boot.inFlight())
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, tg := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.pool.EnsureAgent(context.Background(), tg)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := peak.Load(); p < 1 || p > 2 {
		t.Fatalf("peak bootstraps in flight %d, want at most 2", p)
	}
	for _, tg := range targets {
		if h.pool.Session(tg) == nil {
			t.Fatalf("%s has no session", tg.Pod())
		}
	}
}

// Concurrent ensures of one pod share one bootstrap.
func TestPoolBootstrapsAPodOnce(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	var wg sync.WaitGroup
	sessions := make(chan *AgentSession, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := h.pool.EnsureAgent(context.Background(), target)
			if err != nil {
				t.Error(err)
			}
			sessions <- s
		}()
	}
	wg.Wait()
	close(sessions)
	var first *AgentSession
	for s := range sessions {
		if first == nil {
			first = s
		}
		if s != first {
			t.Fatal("concurrent ensures got different sessions")
		}
	}
	h.wantCounts(target.Pod(), map[string]int{"probe": 1, "stage": 1, "serve": 1})
}

func TestPoolBacksOffAfterAFailedBootstrap(t *testing.T) {
	h := newHarness(t, PoolOptions{BackoffBase: 400 * time.Millisecond, BackoffCap: time.Second, Rand: func() float64 { return 0.99 }})
	target := h.addPod("one")
	h.failMu.Lock()
	h.fail[target.Pod()] = errors.New("apiserver says no")
	h.failMu.Unlock()
	ctx := context.Background()
	if _, err := h.pool.EnsureAgent(ctx, target); err == nil || !strings.Contains(err.Error(), "apiserver says no") {
		t.Fatalf("first ensure: %v", err)
	}
	h.failMu.Lock()
	delete(h.fail, target.Pod())
	h.failMu.Unlock()
	start := time.Now()
	if _, err := h.pool.EnsureAgent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("the retry came %v after the failure, want the ~400ms backoff", d)
	}
}

func TestPoolRefusesAnAgentOfAnotherPod(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	// The pod the exec reaches says it is another (its FLEET_POD_UID): the
	// handshake refuses it.
	orig := h.pool.opts.Exec
	h.pool.opts.Exec = func(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
		if execKind(req.Command) == "serve" {
			p, _ := h.cs.CoreV1().Pods(h.ns).Get(ctx, req.Pod, metav1.GetOptions{})
			p.UID = "impostor"
			_, _ = h.cs.CoreV1().Pods(h.ns).Update(ctx, p, metav1.UpdateOptions{})
			defer func() {
				p.UID = "uid-one"
				_, _ = h.cs.CoreV1().Pods(h.ns).Update(context.Background(), p, metav1.UpdateOptions{})
			}()
		}
		return orig(ctx, c, req, stdio)
	}
	_, err := h.pool.EnsureAgent(context.Background(), target)
	var refused *podwire.RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "not the verified pod") {
		t.Fatalf("err %v, want the agent refused", err)
	}
}

func TestPoolConnections(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	c1, err := h.pool.Cluster(testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Namespace() != h.ns || c1.Conn.Context != "ctx" {
		t.Fatalf("cluster %+v", c1)
	}
	if again, _ := h.pool.Cluster(testCluster); again != c1 {
		t.Fatal("an unchanged connection was reloaded")
	}
	cred, err := h.pool.IssueExecCredential(context.Background(), testCluster, false)
	if err != nil || cred.BearerToken != "tok-one" || cred.ClusterID != testCluster || cred.Server != "https://127.0.0.1:1" {
		t.Fatalf("issued %+v %v", cred, err)
	}

	// A rotated kubeconfig is reloaded once the change is seen.
	writeKubeconfig(t, h.rec.KubeconfigPaths[0], "tok-two-longer")
	h.now.Store(ptrTime(h.pool.opts.Now().Add(stampCheck)))
	c2, err := h.pool.Cluster(testCluster)
	if err != nil || c2 == c1 {
		t.Fatalf("after the kubeconfig changed: %v (same: %v)", err, c2 == c1)
	}
	if cred, _ := h.pool.IssueExecCredential(context.Background(), testCluster, false); cred.BearerToken != "tok-two-longer" {
		t.Fatalf("issued %q after the rotation", cred.BearerToken)
	}
	// The connection's limits outlive the reload.
	if c2.gates != c1.gates {
		t.Fatal("a reload reset the connection's bootstrap limiter")
	}

	// A record change that reaches the cluster another way reloads too; one
	// that does not (bookkeeping) does not.
	rec := h.rec
	rec.VerifiedAt = time.Now()
	h.pool.Configure([]fleet.KubeCluster{rec})
	if c3, _ := h.pool.Cluster(testCluster); c3 != c2 {
		t.Fatal("a bookkeeping change reloaded the connection")
	}
	rec.ExtraPath = []string{"/opt/plugins"}
	h.pool.Configure([]fleet.KubeCluster{rec})
	if c4, _ := h.pool.Cluster(testCluster); c4 == c2 {
		t.Fatal("an extra_path change did not reload the connection")
	}

	// A changed record is picked up without a Configure, once the records
	// are re-read.
	h.rec.Namespace = "elsewhere"
	h.now.Store(ptrTime(h.pool.opts.Now().Add(recordsRecheck)))
	if c5, err := h.pool.Cluster(testCluster); err != nil || c5.Namespace() != "elsewhere" {
		t.Fatalf("a re-read record: %v", err)
	}
	h.rec.Namespace = h.ns
	h.pool.Configure([]fleet.KubeCluster{h.rec})

	// A removed connection drops its sessions.
	target := h.addPod("one")
	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Configure(nil)
	waitDone(t, s.Done(), "a removed connection's session")
	h.rec.ID = "gone"
	h.now.Store(ptrTime(h.pool.opts.Now().Add(recordsRefresh)))
	if _, err := h.pool.Cluster(testCluster); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("a removed connection: %v", err)
	}
	if _, err := h.pool.IssueExecCredential(context.Background(), testCluster, false); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("issue for a removed connection: %v", err)
	}
}

func TestPoolShutdownClosesEverySession(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	a, b := h.addPod("a"), h.addPod("b")
	ctx := context.Background()
	sa, err := h.pool.EnsureAgent(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := h.pool.EnsureAgent(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	bulk, err := sb.Bulk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	h.pool.Shutdown(sctx)
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("shutdown took %v", d)
	}
	for _, done := range []<-chan struct{}{sa.Done(), sb.Done(), bulk.Done()} {
		waitDone(t, done, "a session at shutdown")
	}
	if _, err := h.pool.EnsureAgent(ctx, a); err == nil {
		t.Fatal("a closed pool bootstrapped")
	}
}

// The shutdown's "close the sessions" step ends every session and lets no
// pod have another, but keeps the connection up for what the shutdown still
// does on the cluster (a cancelled create scaling its pod down) until the
// pool itself ends.
func TestPoolCloseSessionsKeepsTheConnection(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	a := h.addPod("a")
	ctx := context.Background()
	s, err := h.pool.EnsureAgent(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	h.pool.CloseSessions(sctx)
	waitDone(t, s.Done(), "the session at CloseSessions")
	if _, err := h.pool.EnsureAgent(ctx, a); err == nil {
		t.Fatal("a pod got a session after CloseSessions")
	}
	if err := h.pool.Scale(ctx, testCluster, a.StatefulSet, 0); err != nil {
		t.Fatalf("the connection went with the sessions: %v", err)
	}
	h.pool.Close()
	if err := h.pool.Scale(ctx, testCluster, a.StatefulSet, 0); err == nil {
		t.Fatal("a closed pool still reached the cluster")
	}
}

// An agent that never answers the Close does not hold the shutdown: the
// sessions close when the step's deadline passes.
func TestPoolCloseSessionsIsBoundedByItsDeadline(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	a := h.addPod("a")
	ctx := context.Background()
	s, err := h.pool.EnsureAgent(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	h.deaf.Store(true)
	sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.pool.CloseSessions(sctx)
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("CloseSessions took %v, want its deadline (300ms)", d)
	}
	waitDone(t, s.Done(), "the deaf pod's session")
}

// A bootstrap someone joins runs at the joiner's priority: a pane opened on
// a pod whose background bootstrap is queued goes ahead of the pods queued
// after it.
func TestPoolJoinedBootstrapTakesTheJoinersPriority(t *testing.T) {
	h := newHarness(t, PoolOptions{MaxBootstraps: 1})
	a, b, c := h.addPod("a"), h.addPod("b"), h.addPod("c")
	b.Priority, c.Priority = PriorityBackground, PriorityInteractive
	unblock := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(unblock) }) })
	blocked := make(chan struct{}, 1)
	var mu sync.Mutex
	var probes []string
	h.onExec = func(e execRecord) {
		if e.kind != "probe" {
			return
		}
		mu.Lock()
		probes = append(probes, e.pod)
		mu.Unlock()
		if e.pod == a.Pod() {
			blocked <- struct{}{}
			<-unblock
		}
	}
	ctx := context.Background()
	errs := make(chan error, 4)
	ensure := func(tg PodTarget) {
		go func() {
			_, err := h.pool.EnsureAgent(ctx, tg)
			errs <- err
		}()
	}
	ensure(a)
	<-blocked // a holds the connection's only slot
	cl, err := h.pool.Cluster(testCluster)
	if err != nil {
		t.Fatal(err)
	}
	ensure(b) // an informer poke: background
	waitQueued(t, cl.gates.boot, 1)
	ensure(c)
	waitQueued(t, cl.gates.boot, 2)
	joined := b
	joined.Priority = PriorityInteractive // then a pane opens on b
	ensure(joined)
	// The join raises b's queued bootstrap before a lets go.
	h.pool.mu.Lock()
	slot := h.pool.slots[podKey{testCluster, h.ns, b.Pod()}]
	h.pool.mu.Unlock()
	slot.mu.Lock()
	tk := slot.flight.ticket
	slot.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tk.mu.Lock()
		raised := tk.prio == PriorityInteractive
		tk.mu.Unlock()
		if raised {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the join did not raise b's bootstrap")
		}
		time.Sleep(time.Millisecond)
	}
	once.Do(func() { close(unblock) })
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{a.Pod(), b.Pod(), c.Pod()}; strings.Join(probes, " ") != strings.Join(want, " ") {
		t.Fatalf("probed %v, want %v", probes, want)
	}
}

// upgrade is the harness's daemon after an upgrade to vtest-2: a new pool
// whose store serves the v2 build, its execs going through exec.
func (h *harness) upgrade(t *testing.T, exec ExecFunc) *Pool {
	t.Helper()
	h.envMu.Lock()
	h.env[binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH)] = fakePod(t, "vtest-2")
	h.envMu.Unlock()
	opts := h.pool.opts
	opts.Bins = &binstore.Store{Dir: t.TempDir(), Self: binstore.Identity{Version: "vtest-2"}, Getenv: h.store.Getenv}
	opts.Version = "vtest-2"
	opts.Rand = func() float64 { return 0.5 }
	if exec != nil {
		opts.Exec = exec
	}
	upgraded := NewPool(opts)
	t.Cleanup(upgraded.Close)
	return upgraded
}

// A lazy restage that fails leaves the pod on its working, older agent: the
// caller gets that session, and the restage is not tried again until the
// backoff has passed.
func TestPoolKeepsTheOlderAgentWhenARestageFails(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	ctx := context.Background()
	if _, err := h.pool.EnsureAgent(ctx, target); err != nil {
		t.Fatal(err)
	}
	var stageFails atomic.Bool
	stageFails.Store(true)
	upgraded := h.upgrade(t, func(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
		if execKind(req.Command) == "stage" && stageFails.Load() {
			_ = h.exec(ctx, c, req, kubeexec.Stdio{Stdout: io.Discard, Stderr: io.Discard})
			return errors.New("no space left on device")
		}
		return h.exec(ctx, c, req, stdio)
	})
	old, err := upgraded.EnsureAgent(ctx, target)
	if err != nil || !old.Stale() {
		t.Fatalf("the upgraded daemon's first session: stale %v, %v", old != nil && old.Stale(), err)
	}
	restage := target
	restage.Restage = true
	got, err := upgraded.EnsureAgent(ctx, restage)
	if err != nil || got != old {
		t.Fatalf("a failed restage: %p %v, want the older agent's session %p", got, err, old)
	}
	before := h.counts(target.Pod())
	if got, err := upgraded.EnsureAgent(ctx, restage); err != nil || got != old {
		t.Fatalf("a restage during the backoff: %p %v", got, err)
	}
	if after := h.counts(target.Pod()); after["stage"] != before["stage"] || after["serve"] != before["serve"] {
		t.Fatalf("a restage during the backoff ran execs: %v, then %v", before, after)
	}
	if out := run(t, old.Client(), "echo still"); out != "still\n" {
		t.Fatalf("the older agent ran %q", out)
	}

	// Once the backoff has passed, the next lazy moment restages.
	stageFails.Store(false)
	h.now.Store(ptrTime(upgraded.opts.Now().Add(DefaultBackoffCap)))
	s, err := upgraded.EnsureAgent(ctx, restage)
	if err != nil || s == old || s.Stale() || s.Hello().Version != "vtest-2" {
		t.Fatalf("the restage after the backoff: %v", err)
	}
}

// A bootstrap that finds the pod no longer verifies (here a restage after a
// volume was added) closes the session it had: a tampered pod gets nothing.
func TestPoolDropsTheSessionOfAPodThatNoLongerVerifies(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	ctx := context.Background()
	if _, err := h.pool.EnsureAgent(ctx, target); err != nil {
		t.Fatal(err)
	}
	upgraded := h.upgrade(t, nil)
	old, err := upgraded.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := h.cs.CoreV1().Pods(h.ns).Get(ctx, target.Pod(), metav1.GetOptions{})
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
	_, _ = h.cs.CoreV1().Pods(h.ns).Update(ctx, p, metav1.UpdateOptions{})
	restage := target
	restage.Restage = true
	var tampered *TamperedError
	if s, err := upgraded.EnsureAgent(ctx, restage); !errors.As(err, &tampered) || s != nil {
		t.Fatalf("restage of a tampered pod: %p %v", s, err)
	}
	waitDone(t, old.Done(), "the tampered pod's session")
	if upgraded.Session(target) != nil {
		t.Fatal("the tampered pod still has a session")
	}
}

// Forget needs no working connection: a destroyed instance's pod is
// forgotten, its session closed, while the kubeconfig does not load.
func TestPoolForgetsAPodWhoseConnectionDoesNotLoad(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.rec.KubeconfigPaths[0], []byte("not: [a kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.now.Store(ptrTime(h.pool.opts.Now().Add(stampCheck)))
	if _, err := h.pool.Cluster(testCluster); err == nil {
		t.Fatal("a broken kubeconfig loaded")
	}
	h.pool.Forget(target)
	waitDone(t, s.Done(), "the forgotten pod's session")
}
