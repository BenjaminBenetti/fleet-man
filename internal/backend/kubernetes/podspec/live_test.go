//go:build !nok8s

package podspec

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// The live tests run the renders against a real API server: its admission
// (pod security, ResourceQuota, LimitRange), its defaulting (the spec hash of
// what it stores is the render's) and its controllers (FailedCreate). Set
// FLEET_KUBE_TEST_CONTEXT to a kube context to run them; each creates and
// deletes fleet-it-p2-05b-* namespaces.

// liveImage is an image a fresh cluster's nodes hold already.
const liveImage = "registry.k8s.io/pause:3.10.2"

type liveCluster struct {
	api    kubernetes.Interface
	prefix string
}

func newLiveCluster(t *testing.T) *liveCluster {
	t.Helper()
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("FLEET_KUBE_TEST_CONTEXT is not set")
	}
	conn, err := kubeexec.Load(fleet.KubeCluster{ID: "live-p205b", Context: kctx, KubeconfigPaths: kubeexec.DefaultKubeconfigPaths(os.Environ()), Namespace: "default"}, kubeexec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	api, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	return &liveCluster{api: api, prefix: "fleet-it-p2-05b-" + names.NewClusterID("x", nil)[2:]}
}

// namespace creates a namespace with labels and objects in it, deleted when
// the test ends.
func (c *liveCluster) namespace(t *testing.T, suffix string, labels map[string]string) string {
	t.Helper()
	ns := c.prefix + "-" + suffix
	ctx := context.Background()
	if _, err := c.api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() { _ = c.api.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	// Admission needs the namespace's default service account.
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := c.api.CoreV1().ServiceAccounts(ns).Get(ctx, "default", metav1.GetOptions{}); err == nil {
			return ns
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace %s has no default service account", ns)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// liveInput is a recorded fixture's input and render input for namespace ns
// of a local cluster.
func liveInput(t *testing.T, fixture, ns string, cluster fleet.KubeCluster) (Input, RenderInput) {
	t.Helper()
	in := loadRecorded(t, fixture).input()
	profiles[0].in(&in)
	cluster.ID, cluster.Namespace = "minikube", ns
	in.Cluster = &cluster
	r := testRenderInput(in)
	r.Image = ImageRef{Ref: liveImage, ID: "sha256:" + strings.Repeat("0", 64)}
	return in, r
}

func TestLiveAdmission(t *testing.T) {
	c := newLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	open := c.namespace(t, "open", nil)
	baseline := c.namespace(t, "baseline", map[string]string{"pod-security.kubernetes.io/enforce": "baseline"})
	restricted := c.namespace(t, "restricted", map[string]string{"pod-security.kubernetes.io/enforce": "restricted"})
	quota := c.namespace(t, "quota", nil)
	if _, err := c.api.CoreV1().ResourceQuotas(quota).Create(ctx, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "small"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("1Gi")}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	limited := c.namespace(t, "limits", nil)
	if _, err := c.api.CoreV1().LimitRanges(limited).Create(ctx, &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "max"},
		Spec:       corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, Max: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	admit := func(fixture, ns string, cluster fleet.KubeCluster, edit func(*Input, *RenderInput)) (*Admitted, error) {
		in, r := liveInput(t, fixture, ns, cluster)
		if edit != nil {
			edit(&in, &r)
		}
		return Admit(ctx, c.api, in, r)
	}
	wantCode := func(name string, err error, code string, mentions ...string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: admitted", name)
			return
		}
		if !strings.HasPrefix(err.Error(), code+": ") {
			t.Errorf("%s: %v, want code %s", name, err, code)
		}
		for _, m := range mentions {
			if !strings.Contains(err.Error(), m) {
				t.Errorf("%s: %v does not mention %q", name, err, m)
			}
		}
		t.Logf("%s: %v", name, err)
	}

	// An unlabelled namespace admits everything a local cluster allows,
	// the debug grants kept.
	for _, fx := range []string{"fixture", "fixture-go-feature", "fixture-dind", "fixture-dood"} {
		a, err := admit(fx, open, fleet.KubeCluster{}, nil)
		if err != nil {
			t.Errorf("%s in %s: %v", fx, open, err)
			continue
		}
		if a.Result.Pod.DebugDropped {
			t.Errorf("%s: debug grants dropped where nothing denies them", fx)
		}
	}

	// Baseline: the go feature's grants are dropped with the warning;
	// privileged is refused; require refuses the grants.
	a, err := admit("fixture-go-feature", baseline, fleet.KubeCluster{}, nil)
	if err != nil {
		t.Fatalf("go feature under baseline: %v", err)
	}
	if !a.Result.Pod.DebugDropped || !strings.Contains(strings.Join(a.Result.Pod.Warnings, "\n"), DebugDroppedWarning) {
		t.Errorf("go feature under baseline: dropped %v, warnings %v", a.Result.Pod.DebugDropped, a.Result.Pod.Warnings)
	}
	_, err = admit("fixture-go-feature", baseline, fleet.KubeCluster{DebugCapabilities: fleet.KubeDebugCapabilitiesRequire}, nil)
	wantCode("go feature under baseline, require", err, CodeAdmissionDenied+":debug_capabilities", "SYS_PTRACE", "set it to auto")
	_, err = admit("fixture-dind", baseline, fleet.KubeCluster{}, nil)
	wantCode("dind under baseline", err, CodeAdmissionDenied, "pod security (baseline)", "privileged")
	// The docker engine's sidecar is privileged too: baseline refuses it,
	// naming the sidecar, never a silently dropped engine.
	_, err = admit("fixture-dood", baseline, fleet.KubeCluster{}, nil)
	wantCode("dood under baseline", err, CodeAdmissionDenied, "pod security (baseline)", DockerSidecar)
	if _, err := admit("fixture", baseline, fleet.KubeCluster{}, nil); err != nil {
		t.Errorf("plain fixture under baseline: %v", err)
	}

	// Restricted: root is refused; a non-root render is admitted.
	_, err = admit("fixture", restricted, fleet.KubeCluster{}, nil)
	wantCode("root under restricted", err, CodeAdmissionDenied, "pod security (restricted)", "runAsNonRoot")
	nonRoot := func(_ *Input, r *RenderInput) {
		r.Identity = Identity{NonRoot: true, UID: ptr(int64(1000)), GID: ptr(int64(1000)), FSGroup: ptr(int64(1000))}
	}
	if _, err := admit("fixture", restricted, fleet.KubeCluster{}, nonRoot); err != nil {
		t.Errorf("non-root under restricted: %v", err)
	}
	// A pod without root cannot be privileged: refused by name, before the
	// API server would call it invalid.
	_, err = admit("fixture-dind", restricted, fleet.KubeCluster{}, nonRoot)
	wantCode("privileged without root", err, CodePrivileged, "without root")

	// no-new-privileges beside SYS_ADMIN is admitted, and kept.
	nnp := baseInput([]string{"--cap-add", "SYS_ADMIN", "--security-opt", "no-new-privileges"}, nil, nil)
	local(&nnp)
	nnp.Cluster.ID, nnp.Cluster.Namespace = "minikube", open
	nnpRender := testRenderInput(nnp)
	nnpRender.Image = ImageRef{Ref: liveImage, ID: "sha256:" + strings.Repeat("0", 64)}
	if a, err := Admit(ctx, c.api, nnp, nnpRender); err != nil {
		t.Errorf("no-new-privileges beside SYS_ADMIN: %v", err)
	} else if ape := devOf(a.Objects).SecurityContext.AllowPrivilegeEscalation; ape == nil || *ape {
		t.Errorf("no-new-privileges beside SYS_ADMIN: allowPrivilegeEscalation %s", jsonString(ape))
	}

	// A quota without room, and a LimitRange maximum under the limit fleet
	// sets when the LimitRange's default would be lower.
	_, err = admit("fixture", quota, fleet.KubeCluster{}, nil)
	wantCode("quota", err, CodeQuota, "requests.memory")
	_, err = admit("fixture", limited, fleet.KubeCluster{}, func(in *Input, _ *RenderInput) { in.Facts.LimitRangeMemoryLimit = "4Gi" })
	wantCode("limit range", err, CodeAdmissionDenied, "LimitRange", "maximum memory usage per Container is 4Gi")

	// The preflight, before any build, is admitted and refused as the
	// real capture is.
	preflight := func(fixture string) func(*Input, *RenderInput) {
		return func(in *Input, _ *RenderInput) {
			rc := loadRecorded(t, fixture)
			capture, err := PreflightCapture(rc.Merged, rc.WorkspaceDir, rc.WorkspaceFolder)
			if err != nil {
				t.Fatal(err)
			}
			in.Capture, in.Image = capture, kubeshim.ImageConfig{}
		}
	}
	a, err = admit("fixture-go-feature", baseline, fleet.KubeCluster{}, preflight("fixture-go-feature"))
	if err != nil || !a.Result.Pod.DebugDropped {
		t.Errorf("go feature preflight under baseline: %v", err)
	}
	_, err = admit("fixture-dind", baseline, fleet.KubeCluster{}, preflight("fixture-dind"))
	wantCode("dind preflight under baseline", err, CodeAdmissionDenied, "privileged")

	// A namespace that is not there.
	_, err = admit("fixture", c.prefix+"-missing", fleet.KubeCluster{}, nil)
	wantCode("missing namespace", err, CodeSettings, "not found")

	// What the API server stores of a render hashes as the render: its
	// defaulting and admission leave the spec hash.
	for _, tt := range []struct {
		fixture, ns string
		edit        func(*Input, *RenderInput)
	}{
		{"fixture-go-feature", open, nil},
		{"fixture-dind", open, nil},
		{"fixture", restricted, nonRoot},
		{"fixture", limited, nil},
	} {
		in, r := liveInput(t, tt.fixture, tt.ns, fleet.KubeCluster{})
		if tt.edit != nil {
			tt.edit(&in, &r)
		}
		res, err := Translate(in)
		if err != nil {
			t.Fatal(err)
		}
		o, err := Render(res, r)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []*corev1.Pod{o.Pod(), o.Seed.Pod()} {
			stored, err := c.api.CoreV1().Pods(tt.ns).Create(ctx, p, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if err != nil {
				t.Errorf("%s in %s: %v", tt.fixture, tt.ns, err)
				continue
			}
			if SpecHash(&stored.Spec) != SpecHash(&p.Spec) {
				t.Errorf("%s in %s: the stored pod %s hashes differently from the render", tt.fixture, tt.ns, p.Name+p.GenerateName)
			}
		}
	}
}

// The real objects: the StatefulSet's pod is the render (its spec hash is
// the annotation), and a pod the namespace refuses is a FailedCreate event
// on its StatefulSet or Job that classifies as the dry-run's refusal.
func TestLiveObjects(t *testing.T) {
	c := newLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	open := c.namespace(t, "objects", nil)
	baseline := c.namespace(t, "events", map[string]string{"pod-security.kubernetes.io/enforce": "baseline"})

	in, r := liveInput(t, "fixture-go-feature", open, fleet.KubeCluster{ProtectFromDrain: true})
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	create := metav1.CreateOptions{FieldManager: FieldManager}
	if _, err := c.api.CoreV1().PersistentVolumeClaims(open).Create(ctx, o.Disk, create); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*corev1.Secret{o.SpecSecret, o.EnvSecret} {
		if _, err := c.api.CoreV1().Secrets(open).Create(ctx, s, create); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.api.PolicyV1().PodDisruptionBudgets(open).Create(ctx, o.DisruptionBudget, create); err != nil {
		t.Fatal(err)
	}
	if _, err := c.api.AppsV1().StatefulSets(open).Create(ctx, o.StatefulSet, create); err != nil {
		t.Fatal(err)
	}
	pod := waitFor(t, ctx, func() (*corev1.Pod, bool) {
		p, err := c.api.CoreV1().Pods(open).Get(ctx, names.Pod(o.StatefulSet.Name), metav1.GetOptions{})
		return p, err == nil
	})
	if got := SpecHash(&pod.Spec); got != o.StatefulSet.Annotations[names.AnnotationSpecHash] {
		t.Errorf("the StatefulSet's pod hashes %s, the annotation says %s", got, o.StatefulSet.Annotations[names.AnnotationSpecHash])
	}
	if pod.Spec.Hostname != pod.Name || pod.Spec.Subdomain != o.StatefulSet.Spec.ServiceName {
		t.Errorf("pod hostname %s.%s", pod.Spec.Hostname, pod.Spec.Subdomain)
	}
	// The seed Job's pod is not the StatefulSet's: no adoption.
	if _, err := c.api.BatchV1().Jobs(open).Create(ctx, o.Seed.Job, create); err != nil {
		t.Fatal(err)
	}
	seedPod := waitFor(t, ctx, func() (*corev1.Pod, bool) {
		l, err := c.api.CoreV1().Pods(open).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + o.Seed.Job.Name})
		if err != nil || len(l.Items) == 0 {
			return nil, false
		}
		return &l.Items[0], true
	})
	for _, ref := range seedPod.OwnerReferences {
		if ref.Kind != "Job" {
			t.Errorf("the seed pod is owned by %s %s", ref.Kind, ref.Name)
		}
	}
	if SpecHash(&seedPod.Spec) != SpecHash(&o.Seed.Pod().Spec) {
		t.Error("the seed pod hashes differently from its render")
	}

	// A re-render of the instance is admitted where a count quota is full
	// with its own objects; another instance is not.
	if _, err := c.api.CoreV1().ResourceQuotas(open).Create(ctx, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "full"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePersistentVolumeClaims: resource.MustParse("1"), corev1.ResourceSecrets: resource.MustParse("2")}},
	}, create); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, func() (struct{}, bool) {
		q, err := c.api.CoreV1().ResourceQuotas(open).Get(ctx, "full", metav1.GetOptions{})
		return struct{}{}, err == nil && len(q.Status.Used) == 2
	})
	if err := DryRun(ctx, c.api, o); err != nil {
		t.Errorf("a re-render of the existing instance: %v", err)
	}
	other := r
	other.InstanceName = "web2"
	other.Key = names.InstanceKey(testDaemonID, testFleet, "web2", testCreatedAt)
	other.Name = names.Instance(testFleet, "web2", other.Key)
	other.Seed.Name = names.SeedJob(other.Name, 1)
	o2, err := Render(res, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := DryRun(ctx, c.api, o2); err == nil || !strings.HasPrefix(err.Error(), CodeQuota+": ") {
		t.Errorf("another instance where the quota is full: %v", err)
	} else {
		t.Logf("another instance: %v", err)
	}

	// FailedCreate: the privileged dind pod under baseline, from its
	// StatefulSet and from a Job.
	in, r = liveInput(t, "fixture-dind", baseline, fleet.KubeCluster{})
	res, err = Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	o, err = Render(res, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.api.AppsV1().StatefulSets(baseline).Create(ctx, o.StatefulSet, create); err != nil {
		t.Fatal(err)
	}
	job := o.Seed.Job.DeepCopy()
	job.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = ptr(true)
	if _, err := c.api.BatchV1().Jobs(baseline).Create(ctx, job, create); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []struct{ kind, name string }{{"StatefulSet", o.StatefulSet.Name}, {"Job", job.Name}} {
		ev := waitFor(t, ctx, func() (*corev1.Event, bool) {
			l, err := c.api.CoreV1().Events(baseline).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + obj.name + ",reason=FailedCreate"})
			if err != nil || len(l.Items) == 0 {
				return nil, false
			}
			return &l.Items[0], true
		})
		d := ClassifyFailedCreate(obj.kind+" "+obj.name, ev.Message)
		if d == nil || d.Kind != DeniedPodSecurity || !d.Policy() || !strings.Contains(d.Error(), "privileged") {
			t.Errorf("%s FailedCreate %q classified %+v", obj.kind, ev.Message, d)
			continue
		}
		t.Logf("%s FailedCreate: %s", obj.kind, d.Error())
	}
}

func waitFor[T any](t *testing.T, ctx context.Context, get func() (T, bool)) T {
	t.Helper()
	for {
		if v, ok := get(); ok {
			return v
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting")
		case <-time.After(250 * time.Millisecond):
		}
	}
}
