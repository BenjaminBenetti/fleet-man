//go:build !nok8s && linux

package kubert

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// live_test.go runs the runtime against a real cluster, in a namespace of its
// own (fleet-it-p5-00-kubert-objects-ensure-wipe-<random>, deleted when
// done):
//
//	FLEET_KUBERT_LIVE=<context>           objects, sibling DNS, the caches
//	                                      and their policies (enforced by a
//	                                      policy-enforcing CNI: calico on
//	                                      fleet-k8s), wipe, upkeep, release,
//	                                      teardown, pending teardowns
//	FLEET_KUBERT_METADATA_LIVE=<context>  the metadata block, against a
//	                                      stand-in metadata server this test
//	                                      puts on the node (169.254.169.254
//	                                      on its loopback, from a privileged
//	                                      hostNetwork pod): only for a
//	                                      throwaway cluster
//
// Both build nothing of fleet's: the pods are busybox.

const liveImage = probeImageForTests

// probeImageForTests is busybox, pinned (the deep test's probe image).
const probeImageForTests = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

type liveRig struct {
	t    *testing.T
	ns   string
	rec  fleet.KubeCluster
	cs   kclient.Interface
	pool *kubernetes.Pool
}

func newLiveRig(t *testing.T, kctx string) *liveRig {
	t.Helper()
	// The kubeconfig is the user's; everything of fleet's is in a home of
	// the test's own.
	paths := kubeexec.DefaultKubeconfigPaths(os.Environ())
	t.Setenv("HOME", t.TempDir())
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	r := &liveRig{t: t, ns: "fleet-it-p5-00-kubert-objects-ensure-wipe-" + hex.EncodeToString(suffix[:])}
	r.rec = fleet.KubeCluster{ID: "live-" + hex.EncodeToString(suffix[:]), DisplayName: "live", Context: kctx,
		KubeconfigPaths: paths, Namespace: r.ns}
	conn, err := kubeexec.Load(r.rec, kubeexec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.cs, err = conn.Clientset(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.cs.CoreV1().Namespaces().Delete(context.Background(), r.ns, metav1.DeleteOptions{}) })
	r.saveRecord()
	rec := r.rec
	r.pool = kubernetes.NewPool(kubernetes.PoolOptions{
		Records: func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil },
		Owner:   func() (string, error) { return testOwner, nil },
	})
	t.Cleanup(r.pool.Close)
	origPool, origDaemon, origUses, origExists := pool, daemonID, fleetUsesCluster, fleetExists
	pool = func() access { return r.pool }
	daemonID = func() (string, error) { return testDaemon, nil }
	fleetUsesCluster = func(string, string) bool { return false }
	fleetExists = func(string) bool { return false }
	t.Cleanup(func() { pool, daemonID, fleetUsesCluster, fleetExists = origPool, origDaemon, origUses, origExists })
	SetHooks(Hooks{})
	t.Cleanup(func() { SetHooks(Hooks{}) })
	return r
}

// saveRecord writes the connection's record where the pool reads its
// current settings (kube_clusters.json in the test's home).
func (r *liveRig) saveRecord() {
	r.t.Helper()
	rec := r.rec
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		if cur := k.Find(rec.ID); cur != nil {
			*cur = rec
			return nil
		}
		k.Clusters = append(k.Clusters, rec)
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *liveRig) meta(fleetName string) fleetMeta {
	return fleetMeta{Namespace: r.ns, Owner: testOwner, Cluster: r.rec.ID, Fleet: fleetName, Hash: names.FleetHash(testDaemon, r.rec.ID, fleetName)}
}

// instance runs an instance-shaped StatefulSet of fleet m (its pod <sts>-0
// labelled as fleet's instance pods are, under the fleet's headless
// Service), serving "<name>" over HTTP on 8080; it returns the pod's name.
func (r *liveRig) instance(m fleetMeta, name string) string {
	r.t.Helper()
	key := names.InstanceKey(testDaemon, m.Fleet, name, time.Unix(1700000000, 0))
	sts := names.Instance(m.Fleet, name, key)
	l := names.Base(m.Owner, m.Cluster)
	l[names.LabelFleet], l[names.LabelKey], l[names.LabelRole], l[names.LabelShareGroup] = m.Hash, key, string(names.RoleInstance), m.Hash
	sel := map[string]string{names.LabelKey: key, names.LabelRole: string(names.RoleInstance)}
	one := int32(1)
	obj := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: sts, Labels: l},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one, ServiceName: m.name(names.SuffixNet),
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec:       r.busybox("mkdir -p /tmp/www && echo " + name + " > /tmp/www/index.html && exec httpd -f -p 8080 -h /tmp/www"),
			},
		},
	}
	if _, err := r.cs.AppsV1().StatefulSets(r.ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		r.t.Fatal(err)
	}
	pod := names.Pod(sts)
	r.waitRunning(pod)
	return pod
}

func (r *liveRig) busybox(script string) corev1.PodSpec {
	zero := int64(0)
	return corev1.PodSpec{
		TerminationGracePeriodSeconds: &zero,
		Containers:                    []corev1.Container{{Name: "box", Image: liveImage, Command: []string{"sh", "-c", script}}},
	}
}

// plainPod runs a pod with no fleet labels (another workload of the
// namespace) and returns its name.
func (r *liveRig) plainPod(name string) string {
	r.t.Helper()
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": "other"}}, Spec: r.busybox("exec sleep 3600")}
	if _, err := r.cs.CoreV1().Pods(r.ns).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		r.t.Fatal(err)
	}
	r.waitRunning(name)
	return name
}

func (r *liveRig) waitRunning(pod string) *corev1.Pod {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		p, err := r.cs.CoreV1().Pods(r.ns).Get(context.Background(), pod, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			return p
		}
		time.Sleep(time.Second)
	}
	r.t.Fatalf("pod %s never ran", pod)
	return nil
}

// sh runs script in pod's one container and returns its output and code.
func (r *liveRig) sh(pod, container, script string) (string, int) {
	r.t.Helper()
	var out bytes.Buffer
	code, err := r.pool.ExecService(context.Background(), r.rec.ID, pod, container, []string{"sh", "-c", script}, &out, &out)
	if err != nil {
		r.t.Fatalf("exec in %s: %v", pod, err)
	}
	return out.String(), code
}

// reaches reports whether pod fetches url within 5s.
func (r *liveRig) reaches(pod, url string) (bool, string) {
	out, code := r.sh(pod, "box", "wget -q -T 5 -O- "+url+" 2>&1")
	return code == 0 || strings.Contains(out, "server returned error"), strings.TrimSpace(out)
}

func (r *liveRig) waitReady(m fleetMeta, svc fleet.SharedService) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		st := readyState(r.rec.ID, m, svc)
		if st == stateReady {
			return
		}
		if strings.HasPrefix(st, "failed") {
			r.t.Fatalf("%s: %s", svc, st)
		}
		time.Sleep(2 * time.Second)
	}
	r.t.Fatalf("%s never came up (%s)", svc, readyState(r.rec.ID, m, svc))
}

func (r *liveRig) objectsOf(m fleetMeta) []string {
	s, err := r.pool.Scope(r.rec.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	got, err := s.ListSelected(context.Background(), m.fleetLabel(), append(append([]string(nil), fleetKinds...), kubernetes.KindPVC))
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// TestLiveKubert: see the file comment.
func TestLiveKubert(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBERT_LIVE")
	if kctx == "" {
		t.Skip("set FLEET_KUBERT_LIVE=<context> to run the Kubernetes runtime against a cluster")
	}
	r := newLiveRig(t, kctx)
	rt := For(r.rec.ID)
	m := r.meta("live")
	ctx := context.Background()
	// step runs a subtest with the rig's helpers failing it, not the test.
	step := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			prev := r.t
			r.t = t
			defer func() { r.t = prev }()
			fn(t)
		})
	}

	if err := rt.PrepareFleet("live"); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureService("live", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureService("live", fleet.ServiceImageCache); err != nil {
		t.Fatal(err)
	}
	a := r.instance(m, "alpha")
	b := r.instance(m, "beta")
	other := r.plainPod("other")
	r.waitReady(m, fleet.ServiceDebCache)
	r.waitReady(m, fleet.ServiceImageCache)
	t.Logf("objects: %v", r.objectsOf(m))

	step("sibling DNS", func(t *testing.T) {
		host := b + "." + m.name(names.SuffixNet)
		ok, out := r.reaches(a, "http://"+host+":8080/")
		if !ok || out != "beta" {
			t.Fatalf("alpha reaching %s: %v %q", host, ok, out)
		}
	})
	step("instances policy", func(t *testing.T) {
		bIP := r.waitRunning(b).Status.PodIP
		if ok, out := r.reaches(other, "http://"+bIP+":8080/"); ok {
			t.Fatalf("a pod outside the fleet reached an instance: %q", out)
		}
		if ok, out := r.reaches(a, "http://"+bIP+":8080/"); !ok {
			t.Fatalf("a sibling did not reach an instance: %q", out)
		}
	})
	step("caches policy", func(t *testing.T) {
		apt := "http://" + m.name(names.SuffixAptCache) + ":3142/acng-report.html"
		if ok, out := r.reaches(a, apt); !ok {
			t.Fatalf("an instance did not reach the deb cache: %q", out)
		}
		if ok, out := r.reaches(other, apt); ok {
			t.Fatalf("a pod outside the fleet reached the deb cache: %q", out)
		}
		mirror := "http://" + m.name(names.SuffixImageCache) + ":5000/v2/"
		if ok, out := r.reaches(a, mirror); !ok {
			t.Fatalf("an instance did not reach the image cache: %q", out)
		}
	})
	step("egress keeps DNS and the rest", func(t *testing.T) {
		if out, code := r.sh(a, "box", "nslookup kubernetes.default.svc.cluster.local"); code != 0 {
			t.Fatalf("DNS from an instance under the egress policy: %s", out)
		}
	})
	step("usage and upkeep", func(t *testing.T) {
		usage, err := rt.CacheUsage(ctx, "live")
		if err != nil || len(usage) != 2 {
			t.Fatalf("usage %+v %v", usage, err)
		}
		for _, u := range usage {
			if u.CapacityBytes <= 0 || u.State != stateReady || u.Location != "live" {
				t.Fatalf("usage %+v", u)
			}
			t.Logf("%s: %d of %d bytes (%d%%), expandable %v", u.Service, u.UsedBytes, u.CapacityBytes, u.Percent(), u.Expandable)
		}
		if err := rt.Maintain(ctx, "live", []fleet.SharedService{fleet.ServiceDebCache, fleet.ServiceImageCache}); err != nil {
			t.Fatal(err)
		}
		readyMu.Lock()
		ran := !aptMaintAt[readyKey(r.rec.ID, m, fleet.ServiceDebCache)].IsZero()
		readyMu.Unlock()
		if !ran {
			t.Fatal("the deb cache's expiry did not run")
		}
	})
	step("wipe", func(t *testing.T) {
		name := m.name(names.SuffixAptCache)
		before, err := r.cs.CoreV1().PersistentVolumeClaims(r.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := rt.WipeService("live", fleet.ServiceDebCache); err != nil {
			t.Fatal(err)
		}
		after, err := r.cs.CoreV1().PersistentVolumeClaims(r.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil || after.UID == before.UID {
			t.Fatalf("volume %q then %q (%v)", before.UID, after.UID, err)
		}
		t.Logf("deb cache wiped and back in %v", time.Since(start).Round(time.Second))
		if ok, out := r.reaches(a, "http://"+m.name(names.SuffixAptCache)+":3142/acng-report.html"); !ok {
			t.Fatalf("the wiped deb cache does not answer: %q", out)
		}
	})
	step("buildkit builder", func(t *testing.T) {
		c, err := rt.connect()
		if err != nil {
			t.Fatal(err)
		}
		// Applied as P5.2 will: the server runs, with its GC under 80% of
		// its volume.
		if err := c.ensureService(ctx, m, fleet.ServiceBuildkit); err != nil {
			t.Fatal(err)
		}
		if err := c.waitAvailable(ctx, m, fleet.ServiceBuildkit); err != nil {
			t.Fatal(err)
		}
		pod, err := c.servicePod(ctx, m, fleet.ServiceBuildkit)
		if err != nil {
			t.Fatal(err)
		}
		var out string
		for i := 0; i < 30; i++ {
			var code int
			out, code = r.sh(pod, string(fleet.ServiceBuildkit), "buildctl debug workers --verbose")
			if code == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}
		// Rule 0 is buildkit's own (local sources, 512MB); the others hold
		// the cache under 80% of the volume's disk.
		limits := matching(out, "Maximum used space")
		if len(limits) < 2 {
			t.Fatalf("buildkitd's GC policy:\n%s", out)
		}
		t.Logf("buildkitd GC limits: %v", limits)
	})
	step("foreign objects", func(t *testing.T) {
		f := r.meta("foreign")
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: f.name(names.SuffixAnchor)}}
		if _, err := r.cs.CoreV1().ConfigMaps(r.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		var fe *ForeignError
		if err := rt.PrepareFleet("foreign"); !errors.As(err, &fe) {
			t.Fatalf("prepare over a foreign object: %v", err)
		}
		got, _ := r.cs.CoreV1().ConfigMaps(r.ns).Get(ctx, cm.Name, metav1.GetOptions{})
		if len(got.Labels) != 0 {
			t.Fatalf("the foreign object was changed: %v", got.Labels)
		}
	})
	step("release and teardown", func(t *testing.T) {
		fleetUsesCluster = func(string, string) bool { return true }
		if err := rt.ReleaseFleet("live"); err != nil {
			t.Fatal(err)
		}
		if got := r.objectsOf(m); len(got) < 10 {
			t.Fatalf("a fleet still on the cluster lost objects: %v", got)
		}
		fleetUsesCluster = func(string, string) bool { return false }
		// Unreachable: owed, not done.
		SetHooks(Hooks{Usable: func(string) bool { return false }})
		var pending *PendingError
		if err := rt.ReleaseFleet("live"); !errors.As(err, &pending) {
			t.Fatalf("release of an unusable cluster = %v", err)
		}
		SetHooks(Hooks{})
		if left := RunPendingTeardowns(ctx, r.rec.ID); left != 0 {
			t.Fatalf("still owed %d", left)
		}
		r.waitObjects(t, m, []string{"pvc/" + m.name(names.SuffixAptCache), "pvc/" + m.name(names.SuffixBuildkit), "pvc/" + m.name(names.SuffixImageCache)})
		// The instances' own objects are not the fleet's to delete.
		if _, err := r.cs.AppsV1().StatefulSets(r.ns).Get(ctx, strings.TrimSuffix(a, "-0"), metav1.GetOptions{}); err != nil {
			t.Fatalf("an instance's StatefulSet went with the fleet's objects: %v", err)
		}
		// A fleet destroy on a connection that deletes the caches with it.
		r.rec.DeleteCacheVolumesOnDestroy = true
		r.saveRecord()
		if err := rt.TeardownFleet("live"); err != nil {
			t.Fatal(err)
		}
		r.waitObjects(t, m, nil)
		if got := PresentClusters("live"); len(got) != 0 {
			t.Fatalf("the destroyed fleet is still recorded on %v", got)
		}
	})
}

// waitObjects waits until the fleet's objects are exactly want (those being
// deleted counting until they are gone).
func (r *liveRig) waitObjects(t *testing.T, m fleetMeta, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var got []string
	for time.Now().Before(deadline) {
		got = r.objectsOf(m)
		if strings.Join(sortedCopy(got), " ") == strings.Join(sortedCopy(want), " ") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("fleet objects %v, want %v", got, want)
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func matching(out, containing string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, containing) {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

// TestLiveMetadataBlock: with a stand-in metadata server on the node, a pod
// outside any fleet reaches it, a fleet's pod does not (DNS still works for
// it), and with the connection's metadata block off the fleet's pod reaches
// it too. Only for a throwaway cluster: it adds 169.254.169.254 to the
// node's loopback.
func TestLiveMetadataBlock(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBERT_METADATA_LIVE")
	if kctx == "" {
		t.Skip("set FLEET_KUBERT_METADATA_LIVE=<throwaway context> to prove the metadata block against a stand-in metadata server")
	}
	r := newLiveRig(t, kctx)
	standIn := r.metadataStandIn()
	t.Logf("stand-in metadata server in pod %s", standIn)

	rt := For(r.rec.ID)
	m := r.meta("meta")
	if err := rt.PrepareFleet("meta"); err != nil {
		t.Fatal(err)
	}
	a := r.instance(m, "alpha")
	other := r.plainPod("other")

	const metadataURL = "http://169.254.169.254/latest/"
	if ok, out := r.reaches(other, metadataURL); !ok {
		t.Fatalf("the stand-in metadata server does not answer a pod outside the fleet: %q", out)
	}
	if ok, out := r.reaches(a, metadataURL); ok {
		t.Fatalf("a fleet pod reached 169.254.169.254 under the egress block: %q", out)
	}
	if out, code := r.sh(a, "box", "nslookup kubernetes.default.svc.cluster.local"); code != 0 {
		t.Fatalf("DNS from a fleet pod under the egress block: %s", out)
	}
	t.Log("blocked: a fleet pod cannot reach 169.254.169.254; a pod outside the fleet can; DNS works")

	off := false
	r.rec.BlockMetadata = &off
	r.saveRecord()
	if err := rt.PrepareFleet("meta"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, out := r.reaches(a, metadataURL)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with the block off a fleet pod still cannot reach the stand-in: %q", out)
		}
		time.Sleep(2 * time.Second)
	}
	t.Log("with block_metadata off the fleet pod reaches the stand-in")
}

// metadataStandIn puts a stand-in metadata server on a node: a privileged
// hostNetwork pod that adds 169.254.169.254 to the node's loopback and
// serves HTTP there (removing the address when it ends).
func (r *liveRig) metadataStandIn() string {
	r.t.Helper()
	yes := true
	zero := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "metadata-stand-in", Labels: map[string]string{"app": "metadata-stand-in"}},
		Spec: corev1.PodSpec{
			HostNetwork:                   true,
			TerminationGracePeriodSeconds: &zero,
			Containers: []corev1.Container{{
				Name: "box", Image: liveImage,
				Command: []string{"sh", "-c", `ip addr add 169.254.169.254/32 dev lo 2>/dev/null || true
trap 'ip addr del 169.254.169.254/32 dev lo' TERM INT EXIT
mkdir -p /tmp/www/latest && echo stand-in > /tmp/www/latest/index.html
httpd -f -p 169.254.169.254:80 -h /tmp/www & wait`},
				SecurityContext: &corev1.SecurityContext{Privileged: &yes},
			}},
		},
	}
	if _, err := r.cs.CoreV1().Pods(r.ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		if apierrors.IsForbidden(err) {
			r.t.Skipf("the namespace does not admit the stand-in's privileged hostNetwork pod: %v", err)
		}
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		_ = r.cs.CoreV1().Pods(r.ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
	})
	r.waitRunning(pod.Name)
	time.Sleep(2 * time.Second)
	return pod.Name
}
