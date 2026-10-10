//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

const (
	handleCluster = "mk-abc234"
	handleOwner   = "handleowner1"
	handleNS      = "fleet"
	handleKey     = "abcd2345"
)

// handleRig is a pool on a fake clientset, with a home of its own.
type handleRig struct {
	t    *testing.T
	cs   *fake.Clientset
	pool *Pool
}

func newHandleRig(t *testing.T, objs ...kruntime.Object) *handleRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: ctx
  context: {cluster: c, user: u, namespace: fleet}
users:
- name: u
  user: {token: tok}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := fleet.KubeCluster{ID: handleCluster, Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: handleNS}
	r := &handleRig{t: t, cs: fake.NewClientset(objs...)}
	r.pool = NewPool(PoolOptions{
		Records:   func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil },
		Environ:   func() []string { return []string{"PATH=/usr/bin:/bin"} },
		Clientset: func(*Cluster) (kclient.Interface, error) { return r.cs, nil },
		Owner:     func() (string, error) { return handleOwner, nil },
	})
	t.Cleanup(r.pool.Close)
	return r
}

// inst is the instance record of web in fleet alpha on the rig's connection.
func (r *handleRig) inst() *fleet.Instance {
	return &fleet.Instance{
		Name: "web", Backend: fleet.BackendKubernetes, KubeCluster: handleCluster, KubeKey: handleKey,
		WorkspaceDir: filepath.Join(state.WorkspacesDir(), "alpha", "web", "alpha"), CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
}

func handleSTS() string { return names.Instance("alpha", "web", handleKey) }

func handleMeta(name string) metav1.ObjectMeta {
	labels := names.Base(handleOwner, handleCluster)
	labels[names.LabelKey] = handleKey
	labels[names.LabelRole] = string(names.RoleInstance)
	return metav1.ObjectMeta{Name: name, Namespace: handleNS, Labels: labels, UID: types.UID("uid-" + name)}
}

func TestFleetOfWorkspace(t *testing.T) {
	root := "/home/u/.fleet/workspaces"
	for _, tc := range []struct {
		ws, inst, want string
		ok             bool
	}{
		{root + "/alpha/web/alpha", "web", "alpha", true},
		{root + "/alpha/web/alpha/", "web", "alpha", true},
		{root + "/alpha/web/beta", "web", "", false},
		{root + "/alpha/other/alpha", "web", "", false},
		{root + "/alpha/web", "web", "", false},
		{"/elsewhere/alpha/web/alpha", "web", "", false},
		{"", "web", "", false},
	} {
		got, ok := fleetOfWorkspace(root, tc.ws, tc.inst)
		if got != tc.want || ok != tc.ok {
			t.Errorf("fleetOfWorkspace(%q, %q) = %q, %v; want %q, %v", tc.ws, tc.inst, got, ok, tc.want, tc.ok)
		}
	}
}

// The handle names the instance's objects from its record: its fleet comes
// from the workspace path, else from the state record holding its key.
func TestHandleIdentity(t *testing.T) {
	r := newHandleRig(t)
	b := newHandle(r.inst(), r.pool)
	id, err := b.identity()
	if err != nil || id.fleet != "alpha" || id.sts != handleSTS() || id.cluster != handleCluster || !id.created.Equal(r.inst().CreatedAt) {
		t.Fatalf("identity = %+v, %v", id, err)
	}

	moved := r.inst()
	moved.WorkspaceDir = "/somewhere/else"
	if _, err := newHandle(moved, r.pool).identity(); err == nil {
		t.Fatal("a record whose fleet nothing names has an identity")
	}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"beta": {Name: "beta", Instances: []*fleet.Instance{moved}}}}); err != nil {
		t.Fatal(err)
	}
	if id, err := newHandle(moved, r.pool).identity(); err != nil || id.fleet != "beta" {
		t.Fatalf("identity from state = %+v, %v", id, err)
	}

	for name, mut := range map[string]func(*fleet.Instance){
		"no cluster": func(i *fleet.Instance) { i.KubeCluster = "" },
		"no key":     func(i *fleet.Instance) { i.KubeKey = "" },
		"bad key":    func(i *fleet.Instance) { i.KubeKey = "NOT-A-KEY" },
	} {
		inst := r.inst()
		mut(inst)
		if _, err := newHandle(inst, r.pool).identity(); !errors.Is(err, errUnbound) {
			t.Errorf("%s: identity error %v", name, err)
		}
	}
}

func TestWhere(t *testing.T) {
	r := newHandleRig(t)
	inst := r.inst()
	b := newHandle(inst, r.pool)
	w, err := b.where(ContainerID("other-cluster", "ns2", "fi-x-y-zzzz2345", 3))
	if err != nil || w != (where{cluster: "other-cluster", namespace: "ns2", sts: "fi-x-y-zzzz2345", gen: 3}) {
		t.Fatalf("where(id) = %+v, %v", w, err)
	}
	if w, err := b.where(""); err != nil || w.cluster != handleCluster || w.sts != handleSTS() || w.gen != 1 {
		t.Fatalf("where(\"\") = %+v, %v", w, err)
	}
	if _, err := b.where("0123456789ab"); err == nil {
		t.Fatal("a docker container id is a Kubernetes one")
	}
}

// What the handle cannot do yet it refuses with kubernetes:unavailable,
// naming it; what it never does without I/O it answers at once.
func TestHandleCapabilities(t *testing.T) {
	b := newHandle((&handleRig{}).inst(), nil)
	if !b.SupportsSharedMounts() || b.SupportsClone() || b.SupportsRebuild() || b.SupportsMicSink() {
		t.Fatal("capabilities")
	}
	if !backend.HasControlChannel(b) {
		t.Fatal("no control channel: provisioning would mount a host control directory")
	}
	unavailableCode := func(what string, err error) {
		t.Helper()
		if code, _ := kubeerr.Of(err); code != kubeerr.Unavailable {
			t.Errorf("%s: %v, want kubernetes:unavailable", what, err)
		}
	}
	_, err := b.Clone("cid", "/ws", nil)
	unavailableCode("Clone", err)
	_, err = b.Rebuild("cid", "/ws", nil)
	unavailableCode("Rebuild", err)
	unavailableCode("Exec", b.Exec("/ws", []string{"true"}))
	unavailableCode("ExecCommand", b.ExecCommand("/ws", []string{"true"}).Run())
	unavailableCode("ExecCommandQuiet", b.ExecCommandQuiet("/ws", []string{"true"}).Run())
	unavailableCode("CopyFile", b.CopyFile("/ws", strings.NewReader("x"), "/tmp/x", 0o644))
	unavailableCode("LogsCommand", b.LogsCommand("cid", false).Run())
	if uri, ok := b.EditorURI("/ws", "p"); ok || uri != "" {
		t.Error("an editor URI")
	}
	if cmd, ok := b.MicSinkCommand("cid"); ok || cmd != nil {
		t.Error("a mic sink")
	}
	if host, ok := b.ResolveHostname("cid"); ok || host != "" {
		t.Error("a reachable hostname")
	}
}

// The pod the supervisor keeps an agent up in is fleetd's record of it.
func TestPodTargetFromTheRecord(t *testing.T) {
	r := newHandleRig(t)
	b := newHandle(r.inst(), r.pool)
	if _, err := b.PodTarget(); !errors.Is(err, errNoInstanceRecord) {
		t.Fatalf("without a record: %v", err)
	}
	rec := InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web",
		StatefulSet: handleSTS(), StatefulSetUID: "uid-sts", Gen: 2, SpecHash: "sha256:h", ImageDigests: []string{"sha256:" + strings.Repeat("a", 64)}}
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	tgt, err := b.PodTarget()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Cluster != handleCluster || tgt.StatefulSet != handleSTS() || tgt.Fleet != "alpha" || tgt.Instance != "web" ||
		tgt.SpecSecret != names.SpecSecret(handleSTS(), 2) || tgt.Expect.SpecHash != "sha256:h" || tgt.Expect.StatefulSetUID != "uid-sts" ||
		len(tgt.Expect.ImageDigests) != 1 {
		t.Fatalf("target %+v", tgt)
	}
}

func TestInstanceRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web",
		StatefulSet: handleSTS(), StatefulSetUID: "u", Gen: 1, SpecHash: "sha256:h", Image: "img@sha256:x", Strategy: "passthrough",
		RemoteUser: "vscode", WorkspaceFolder: "/workspaces/alpha", Created: time.Unix(1700000000, 5).UTC()}
	rec.addImageDigest("sha256:a")
	rec.addImageDigest("sha256:a")
	rec.addImageDigest("")
	if len(rec.ImageDigests) != 1 {
		t.Fatalf("digests %v", rec.ImageDigests)
	}
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	path := InstanceRecordPath(handleCluster, handleKey)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record file %v, %v", info, err)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("record dir %v, %v", dir, err)
	}
	got, err := LoadInstanceRecord(handleCluster, handleKey)
	rec.V = InstanceRecordVersion
	if err != nil || fmt.Sprint(got) != fmt.Sprint(rec) {
		t.Fatalf("round trip\n got %+v\nwant %+v\n%v", got, rec, err)
	}
	// Another connection's file under this one's name is not its record.
	other := rec
	other.Cluster = "other-abc234"
	if err := SaveInstanceRecord(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(InstanceRecordPath("other-abc234", handleKey), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstanceRecord(handleCluster, handleKey); err == nil {
		t.Error("a record naming another connection loaded")
	}
	if err := os.WriteFile(path, []byte(`{"v":99,"cluster":"`+handleCluster+`","key":"`+handleKey+`","statefulSet":"s","gen":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstanceRecord(handleCluster, handleKey); !errors.Is(err, ErrInstanceRecordNewer) {
		t.Errorf("a newer record: %v", err)
	}
	if err := RemoveInstanceRecord(handleCluster, handleKey); err != nil {
		t.Fatal(err)
	}
	if err := RemoveInstanceRecord(handleCluster, handleKey); err != nil {
		t.Fatalf("removing a missing record: %v", err)
	}
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Key: "bad", StatefulSet: "s", Gen: 1}); err == nil {
		t.Error("a record with a bad key saved")
	}
}

// A stop scales the StatefulSet to 0 under fleet's field manager and waits
// for the pod to go.
func TestStopScalesToZeroAndWaits(t *testing.T) {
	sts := handleSTS()
	one := int32(1)
	r := newHandleRig(t,
		&appsv1.StatefulSet{ObjectMeta: handleMeta(sts), Spec: appsv1.StatefulSetSpec{Replicas: &one}},
		&corev1.Pod{ObjectMeta: handleMeta(names.Pod(sts))},
	)
	// The controller: the pod goes a moment after the scale to 0.
	r.cs.PrependReactor("patch", "statefulsets", func(a k8stesting.Action) (bool, kruntime.Object, error) {
		p := a.(k8stesting.PatchAction)
		if strings.Contains(string(p.GetPatch()), `"replicas":0`) {
			go func() {
				time.Sleep(200 * time.Millisecond)
				_ = r.cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, handleNS, names.Pod(sts))
			}()
		}
		return false, nil, nil
	})
	b := newHandle(r.inst(), r.pool)
	cid := ContainerID(handleCluster, handleNS, sts, 1)
	if err := b.Stop(cid); err != nil {
		t.Fatal(err)
	}
	set, _ := r.cs.AppsV1().StatefulSets(handleNS).Get(context.Background(), sts, metav1.GetOptions{})
	if *set.Spec.Replicas != 0 {
		t.Fatalf("replicas %d", *set.Spec.Replicas)
	}
	var managers []string
	for _, a := range r.cs.Actions() {
		if p, ok := a.(k8stesting.PatchActionImpl); ok && p.GetResource().Resource == "statefulsets" {
			managers = append(managers, p.PatchOptions.FieldManager)
		}
	}
	if len(managers) != 1 || managers[0] != DefaultFieldManager {
		t.Fatalf("patches by %v", managers)
	}
	if _, err := r.cs.CoreV1().Pods(handleNS).Get(context.Background(), names.Pod(sts), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the pod is still there: %v", err)
	}
	// A StatefulSet that is gone is the cluster's to report.
	if err := newHandle(r.inst(), r.pool).Stop(ContainerID(handleCluster, handleNS, "fi-gone-x-zzzz2345", 1)); err == nil {
		t.Fatal("stopping a missing StatefulSet")
	}
}

// A teardown tombstones the key, deletes every object of it, releases the
// image the instance held and forgets fleetd's record of it; DownByKey of a
// record without a key has nothing to do.
func TestTeardown(t *testing.T) {
	sts := handleSTS()
	r := newHandleRig(t,
		&appsv1.StatefulSet{ObjectMeta: handleMeta(sts)},
		&corev1.PersistentVolumeClaim{ObjectMeta: handleMeta(sts)},
		&corev1.Secret{ObjectMeta: handleMeta(names.SpecSecret(sts, 1))},
		&batchv1.Job{ObjectMeta: handleMeta(names.SeedJob(sts, 1))},
	)
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Key: handleKey, StatefulSet: sts, Gen: 1}); err != nil {
		t.Fatal(err)
	}
	// The image the instance holds, in the connection's index.
	idx := kubeimage.Index{Version: 1, Images: []kubeimage.Entry{{ImageID: "sha256:" + strings.Repeat("1", 64), Strategy: kubeimage.StrategyPassthrough,
		Location: "mcr.microsoft.com/x", Ref: "mcr.microsoft.com/x@sha256:" + strings.Repeat("2", 64), Holders: []string{handleKey}, DeliveredAt: time.Now()}}}
	writeIndex(t, state.KubeImagesPath(handleCluster), idx)
	images := &kubeimage.Manager{IndexPath: state.KubeImagesPath}
	SetEnvironment(Environment{Images: images})
	t.Cleanup(func() { SetEnvironment(Environment{}) })

	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, sts, 1)
	if err := newHandle(inst, r.pool).Down(inst.ContainerID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"statefulsets", "persistentvolumeclaims", "secrets", "jobs"} {
		if n := countDeletes(r.cs, kind); n != 1 {
			t.Errorf("%d deletes of %s", n, kind)
		}
	}
	fp, err := LoadFootprint()
	if err != nil || len(fp.TombstonesOf(handleCluster)) != 1 || fp.TombstonesOf(handleCluster)[0].Key != handleKey || fp.TombstonesOf(handleCluster)[0].Fleet != "alpha" {
		t.Errorf("tombstones %+v, %v", fp, err)
	}
	if d, err := images.Lookup(handleCluster, handleKey); err != nil || d != nil {
		t.Errorf("the instance still holds its image: %+v, %v", d, err)
	}
	if _, err := LoadInstanceRecord(handleCluster, handleKey); !errors.Is(err, errNoInstanceRecord) {
		t.Errorf("the record outlived the teardown: %v", err)
	}

	keyless := r.inst()
	keyless.KubeKey = ""
	before := len(r.cs.Actions())
	if err := newHandle(keyless, r.pool).DownByKey(); err != nil || len(r.cs.Actions()) != before {
		t.Errorf("DownByKey of a keyless record: %v (%d API calls)", err, len(r.cs.Actions())-before)
	}
}

func countDeletes(cs *fake.Clientset, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteActionImpl); ok && d.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func writeIndex(t *testing.T, path string, idx kubeimage.Index) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Without an agent session the polls report nothing (they never start one)
// and control files wait; a control file is named by its base name only.
func TestPollsWithoutASession(t *testing.T) {
	r := newHandleRig(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)
	if out := b.ListSessions(inst.ContainerID); out != "" {
		t.Errorf("sessions %q", out)
	}
	if s := b.CaptureAllSessions(inst.ContainerID); s.OK {
		t.Error("a capture without a session")
	}
	if _, ok := b.AgentToolProbe(inst.ContainerID); ok {
		t.Error("a tool probe without a session")
	}
	if err := b.WriteControlFile("status.json", []byte("{}"), 0o644); !errors.Is(err, errNoSession) {
		t.Errorf("a control file without a session: %v", err)
	}
	for _, name := range []string{"", ".", "..", "a/b", "../x", "nul\x00"} {
		if err := b.WriteControlFile(name, nil, 0o644); err == nil || errors.Is(err, errNoSession) {
			t.Errorf("control file %q: %v", name, err)
		}
		if err := b.RemoveControlFile(name); err == nil || errors.Is(err, errNoSession) {
			t.Errorf("removing control file %q: %v", name, err)
		}
	}
	if len(r.cs.Actions()) != 0 {
		t.Errorf("the polls called the API server: %v", r.cs.Actions())
	}
	if st := b.Status(inst.ContainerID); st != backend.LiveStatusUnknown {
		t.Errorf("status of an unwatched instance %s", st)
	}
	if _, ok := b.LastRestart(inst.ContainerID); ok {
		t.Error("a restart nobody saw")
	}
}

// A Job's pod gets the workspace only when it is the pod fleetd rendered,
// running the image fleetd delivered.
func TestCheckJobPod(t *testing.T) {
	want := corev1.PodSpec{Containers: []corev1.Container{{Name: podspec.JobContainer, Image: "img", Command: []string{"sh"}}}}
	digest := "sha256:" + strings.Repeat("d", 64)
	pod := func(spec corev1.PodSpec, image, imageID string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}, Spec: spec,
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: podspec.JobContainer, Image: image, ImageID: imageID}}}}
	}
	changed := *want.DeepCopy()
	changed.Containers[0].Command = []string{"sh", "-c", "steal"}
	var tampered *TamperedError
	for name, tc := range map[string]struct {
		pod *corev1.Pod
		ok  bool
	}{
		"rendered, delivered digest": {pod(want, "repo@"+digest, "repo@"+digest), true},
		"not started yet":            {pod(want, "", ""), true},
		"node-loaded, its own name":  {pod(want, "fleet.invalid/x/devcontainer:abc", "sha256:"+strings.Repeat("e", 64)), true},
		"another spec":               {pod(changed, "repo@"+digest, "repo@"+digest), false},
		"another image":              {pod(want, "evil", "sha256:"+strings.Repeat("f", 64)), false},
	} {
		err := checkJobPod(tc.pod, &want, "fleet.invalid/x/devcontainer:abc", []string{digest})
		if tc.ok != (err == nil) || (err != nil && !errors.As(err, &tampered)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSameImage(t *testing.T) {
	for _, tc := range []struct {
		status, ref string
		want        bool
	}{
		{"fleet.invalid/x/devcontainer:abc", "fleet.invalid/x/devcontainer:abc", true},
		{"docker.io/library/alpine:3", "alpine:3", true},
		{"docker.io/me/app:1", "me/app:1", true},
		{"alpine:3", "alpine:4", false},
		{"", "alpine:3", false},
		{"alpine:3", "", false},
	} {
		if got := sameImage(tc.status, tc.ref); got != tc.want {
			t.Errorf("sameImage(%q, %q) = %v", tc.status, tc.ref, got)
		}
	}
}

// The id a node gives a node-loaded image is learned from the instance's
// first pod; any other image the pod runs is refused.
func TestLearnNodeImage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	digest := "sha256:" + strings.Repeat("c", 64)
	running := func(image, imageID string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: DevContainer, Image: image, ImageID: imageID}}}}
	}
	c := &creation{}
	nodeLoad := &kubeimage.Delivery{Strategy: kubeimage.StrategyNodeLoad, Ref: "fleet.invalid/d/devcontainer:abc"}
	rec := InstanceRecord{Cluster: handleCluster, Key: handleKey, StatefulSet: handleSTS(), Gen: 1, ImageDigests: []string{"sha256:" + strings.Repeat("1", 64)}}
	if err := c.learnNodeImage(running(nodeLoad.Ref, digest), &rec, nodeLoad); err != nil {
		t.Fatal(err)
	}
	if len(rec.ImageDigests) != 2 || rec.ImageDigests[1] != digest {
		t.Fatalf("digests %v", rec.ImageDigests)
	}
	if saved, err := LoadInstanceRecord(handleCluster, handleKey); err != nil || len(saved.ImageDigests) != 2 {
		t.Fatalf("saved %+v, %v", saved, err)
	}
	var tampered *TamperedError
	if err := c.learnNodeImage(running("evil:1", "sha256:"+strings.Repeat("e", 64)), &rec, nodeLoad); !errors.As(err, &tampered) {
		t.Errorf("another image on a node-loaded instance: %v", err)
	}
	registry := &kubeimage.Delivery{Strategy: kubeimage.StrategyRegistry, Ref: "reg/fleet/x@" + digest, Digest: digest}
	rec2 := InstanceRecord{Cluster: handleCluster, Key: handleKey, StatefulSet: handleSTS(), Gen: 1, ImageDigests: []string{digest}}
	if err := c.learnNodeImage(running(registry.Ref, "reg/fleet/x@"+digest), &rec2, registry); err != nil || len(rec2.ImageDigests) != 1 {
		t.Errorf("the delivered digest: %v %v", err, rec2.ImageDigests)
	}
	if err := c.learnNodeImage(running(registry.Ref, "reg/fleet/x@sha256:"+strings.Repeat("9", 64)), &rec2, registry); !errors.As(err, &tampered) {
		t.Errorf("another digest from a registry: %v", err)
	}
}

func TestInstanceIdentity(t *testing.T) {
	if id := instanceIdentity(nil); id.NonRoot {
		t.Error("no capabilities: root")
	}
	if id := instanceIdentity(&fleetgrpc.KubeCapabilities{Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}); id.NonRoot {
		t.Error("root admitted: root")
	}
	id := instanceIdentity(&fleetgrpc.KubeCapabilities{Admission: &fleetgrpc.KubeAdmission{}})
	uid, gid := shareIDs()
	if !id.NonRoot || id.UID == nil || *id.UID != int64(uid) || id.GID == nil || *id.GID != int64(gid) || uid == 0 {
		t.Errorf("root refused: %+v (share ids %d:%d)", id, uid, gid)
	}
}

func TestClusterFacts(t *testing.T) {
	f := clusterFacts(Facts{Local: localcluster.Cluster{Kind: localcluster.Minikube, Name: "mk"}, Capabilities: &fleetgrpc.KubeCapabilities{
		LimitRangeMemoryLimit: "4Gi", RuntimeClasses: []string{"sysbox"},
		Nodes: []*fleetgrpc.KubeNode{{Arch: "arm64", AllocatableCpu: "4", AllocatableMemory: "16Gi"}},
	}})
	if !f.Local || f.LimitRangeMemoryLimit != "4Gi" || len(f.RuntimeClasses) != 1 || len(f.Nodes) != 1 || f.Nodes[0].Arch != "arm64" || f.Nodes[0].AllocatableMemory != "16Gi" {
		t.Errorf("facts %+v", f)
	}
	if f := clusterFacts(Facts{}); f.Local || len(f.Nodes) != 0 {
		t.Errorf("no facts %+v", f)
	}
}

func TestImageRefOf(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	got := imageRefOf(&kubeimage.Delivery{Ref: "r@" + digest, Digest: digest, ImageID: "sha256:host", PullPolicy: kubeimage.PullIfNotPresent, PullSecrets: []string{"s"}})
	if got.Ref != "r@"+digest || got.ID != digest || got.PullPolicy != "" || len(got.PullSecrets) != 1 {
		t.Errorf("registry: %+v", got)
	}
	got = imageRefOf(&kubeimage.Delivery{Ref: "fleet.invalid/d/devcontainer:x", ImageID: "sha256:host", PullPolicy: kubeimage.PullNever})
	if got.ID != "sha256:host" || got.PullPolicy != corev1.PullNever {
		t.Errorf("node load: %+v", got)
	}
}

// The workspace travels as the tar the seed extracts: what the seed writes
// is the checkout, files, dirs, modes and symlinks kept.
func TestWorkspaceTar(t *testing.T) {
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, ".git", "objects"), 0o755))
	must(os.WriteFile(filepath.Join(src, "README.md"), []byte("# hi\n"), 0o644))
	must(os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(os.WriteFile(filepath.Join(src, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	must(os.Symlink("README.md", filepath.Join(src, "link")))
	must(os.Symlink("/etc/passwd", filepath.Join(src, "abs")))
	if l, err := net.Listen("unix", filepath.Join(src, "sock")); err == nil {
		defer l.Close()
	}

	r, err := workspaceTar(src)()
	must(err)
	dst := t.TempDir()
	st, err := podseed.ExtractWorkspace(r, dst, nil, nil)
	must(err)
	must(r.Close())
	if st.Files < 3 {
		t.Errorf("extracted %+v", st)
	}
	for rel, want := range map[string]string{"README.md": "# hi\n", "run.sh": "#!/bin/sh\n", ".git/HEAD": "ref: refs/heads/main\n"} {
		if got, err := os.ReadFile(filepath.Join(dst, rel)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", rel, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode %v, %v", info, err)
	}
	for link, target := range map[string]string{"link": "README.md", "abs": "/etc/passwd"} {
		if got, err := os.Readlink(filepath.Join(dst, link)); err != nil || got != target {
			t.Errorf("%s -> %q, %v", link, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "sock")); !os.IsNotExist(err) {
		t.Errorf("a socket travelled: %v", err)
	}
	if _, err := workspaceTar(filepath.Join(src, "README.md"))(); err == nil {
		t.Error("a tar of a file")
	}
}

// Every code the backend's packages make is a kubernetes: code the recovery
// dialog knows the class of.
func TestEveryCodeIsAKubeerrClass(t *testing.T) {
	for _, code := range []string{
		CodeTampered, CodeOOMKilled, CodeCrashLoop, CodePVCPending, CodeUnschedulable, CodeImagePull, CodeTimeout,
		kubeimage.CodeNoDelivery, kubeimage.CodeImagePush, kubeimage.CodeImagePull,
		podspec.CodeHostSpecialFile, podspec.CodeHostMount, podspec.CodePrivileged, podspec.CodeNoCommand, podspec.CodeSpecTooLarge,
		podspec.CodeSettings, podspec.CodeAdmissionDenied, podspec.CodeUnsupportedMount, podspec.CodeQuota, podspec.CodeForbidden, podspec.CodeInvalidSpec,
		kubeshim.UnsupportedCompose, kubeshim.UnsupportedFlag("--link"), notYet{what: "x"}.Code(),
	} {
		if !kubeerr.Code(code).Valid() {
			t.Errorf("%q is not a kubeerr class", code)
		}
	}
}

func TestAPICode(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	for name, tc := range map[string]struct {
		err  error
		want kubeerr.Code
	}{
		"unauthorized":   {apierrors.NewUnauthorized("expired"), kubeerr.Unauthorized},
		"forbidden":      {apierrors.NewForbidden(gr, "x", errors.New("no")), kubeerr.Forbidden},
		"quota":          {apierrors.NewForbidden(gr, "x", errors.New("exceeded quota: q, requested: pods=1")), kubeerr.Quota},
		"unavailable":    {apierrors.NewServiceUnavailable("down"), kubeerr.Unreachable},
		"dial":           {&net.OpError{Op: "dial", Err: errors.New("refused")}, kubeerr.Unreachable},
		"connection":     {fmt.Errorf("pool: %w", ErrClusterNotFound), kubeerr.ClusterMissing},
		"kubeconfig":     {fmt.Errorf("load: %w", kubeexec.ErrContextMissing), kubeerr.Misconfigured},
		"owner":          {ErrOwnerConflict, kubeerr.OwnerConflict},
		"deadline":       {context.DeadlineExceeded, kubeerr.Timeout},
		"cancelled":      {context.Canceled, ""},
		"something else": {errors.New("exit status 1"), ""},
	} {
		if got := apiCode(tc.err); got != tc.want {
			t.Errorf("%s: apiCode = %q, want %q", name, got, tc.want)
		}
	}
	if err := classify(errors.New("exit status 1")); err.Error() != "exit status 1" {
		t.Errorf("an error with no code gained one: %v", err)
	}
	coded := &DeadlineError{Kind: CodePVCPending, Wait: "the disk", After: time.Minute}
	if got := classify(coded); got != error(coded) {
		t.Errorf("a coded error was re-coded: %v", got)
	}
	if code, _ := kubeerr.Of(classify(apierrors.NewUnauthorized("x"))); code != kubeerr.Unauthorized {
		t.Errorf("classified unauthorized: %s", code)
	}
}

func TestReportWarningsGoOnTheBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &creation{id: bound{fleet: "alpha", name: "web"}, warnings: []string{"debug capabilities dropped by admission", " ", "debug capabilities dropped by admission", "second"}}
	c.reportWarnings()
	data, err := os.ReadFile(state.WarnPath("alpha", "web"))
	if err != nil || !bytes.Equal(data, []byte("debug capabilities dropped by admission\nsecond")) {
		t.Errorf("banner %q, %v", data, err)
	}
}

// A start whose pod does not come up fails with why, and leaves the
// instance stopped: scaled back to 0, not left crash looping.
func TestStartThatFailsScalesBackDown(t *testing.T) {
	sts := handleSTS()
	zero := int32(0)
	r := newHandleRig(t, &appsv1.StatefulSet{ObjectMeta: handleMeta(sts), Spec: appsv1.StatefulSetSpec{Replicas: &zero}},
		&corev1.PersistentVolumeClaim{ObjectMeta: handleMeta(sts), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}})
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Key: handleKey, StatefulSet: sts, Gen: 1, Fleet: "alpha", Instance: "web"}); err != nil {
		t.Fatal(err)
	}
	// The controller makes a pod that keeps crashing.
	r.cs.PrependReactor("patch", "statefulsets", func(a k8stesting.Action) (bool, kruntime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"replicas":1`) {
			pod := &corev1.Pod{ObjectMeta: handleMeta(names.Pod(sts)), Status: corev1.PodStatus{Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: DevContainer, RestartCount: 3,
					State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}}}}}}
			_ = r.cs.Tracker().Add(pod)
		}
		return false, nil, nil
	})
	b := newHandle(r.inst(), r.pool)
	err := b.Start(ContainerID(handleCluster, handleNS, sts, 1))
	if code, _ := kubeerr.Of(err); code != kubeerr.CrashLoop {
		t.Fatalf("start of a crash-looping instance: %v", err)
	}
	set, _ := r.cs.AppsV1().StatefulSets(handleNS).Get(context.Background(), sts, metav1.GetOptions{})
	if *set.Spec.Replicas != 0 {
		t.Fatalf("the failed start left replicas at %d", *set.Spec.Replicas)
	}
}
