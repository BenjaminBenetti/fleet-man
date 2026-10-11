//go:build !nok8s

package kubert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

const (
	testCluster = "minikube-abc234"
	testDaemon  = "0123456789abcdef"
	testOwner   = "0123456789ab"
)

// fakePool is the pool as the runtime's tests see it: one connection on a
// fake clientset, execs answered by exec.
type fakePool struct {
	cs   *fake.Clientset
	rec  fleet.KubeCluster
	gate func(context.Context) error
	// scopeErr, when set, is the connection's client not loading (its
	// record still reads).
	scopeErr error

	mu    sync.Mutex
	execs [][]string
	exec  func(argv []string) (string, int)
}

func (f *fakePool) Scope(id string) (kubernetes.Scope, error) {
	if id != testCluster {
		return kubernetes.Scope{}, kubernetes.ErrClusterNotFound
	}
	if f.scopeErr != nil {
		return kubernetes.Scope{}, f.scopeErr
	}
	return kubernetes.Scope{Client: f.cs, Namespace: "fleet", Owner: testOwner, Cluster: testCluster, Gate: f.gate}, nil
}

func (f *fakePool) Record(id string) (fleet.KubeCluster, error) {
	if id != testCluster {
		return fleet.KubeCluster{}, kubernetes.ErrClusterNotFound
	}
	return f.rec, nil
}

func (f *fakePool) ExecService(_ context.Context, _, _, _ string, argv []string, stdout, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.execs = append(f.execs, argv)
	run := f.exec
	f.mu.Unlock()
	if run == nil {
		return 0, nil
	}
	out, code := run(argv)
	_, _ = io.WriteString(stdout, out)
	return code, nil
}

func (f *fakePool) execCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.execs {
		if strings.HasPrefix(strings.Join(a, " "), prefix) {
			n++
		}
	}
	return n
}

// useFake points the runtime at a fake connection, footprint and state.
func useFake(t *testing.T, objs ...runtime.Object) *fakePool {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f := &fakePool{cs: fake.NewClientset(objs...), rec: fleet.KubeCluster{ID: testCluster, Namespace: "fleet", DisplayName: "minikube"}}
	giveUIDs(f.cs)
	origPool, origDaemon, origFacts, origUses, origExists, origKeys := pool, daemonID, connectionFacts, fleetUsesCluster, fleetExists, fleetInstanceKeys
	pool = func() access { return f }
	daemonID = func() (string, error) { return testDaemon, nil }
	connectionFacts = func(context.Context, string) (kubernetes.Facts, error) {
		return kubernetes.Facts{}, errors.New("no facts")
	}
	// Every fleet has an instance on the connection unless a test says
	// otherwise (a release, a pending teardown, a purge).
	fleetUsesCluster = func(string, string) bool { return true }
	fleetExists = func(string) bool { return false }
	// Each fleet's instances "i1" and "i2" are on the connection, keyed
	// "key-<fleet>-<instance>".
	fleetInstanceKeys = func(fleetName, _ string) (map[string]string, error) {
		return map[string]string{"i1": "key-" + fleetName + "-i1", "i2": "key-" + fleetName + "-i2"}, nil
	}
	fp := filepath.Join(t.TempDir(), "footprint.json")
	origFP := kubernetes.FootprintPath
	kubernetes.FootprintPath = func() string { return fp }
	origPoll, origGone := pollInterval, goneTimeout
	pollInterval, goneTimeout = time.Millisecond, time.Second
	SetHooks(Hooks{})
	stopWatches := watchFor(t)
	t.Cleanup(func() {
		stopWatches()
		pool, daemonID, connectionFacts, fleetUsesCluster, fleetExists, fleetInstanceKeys = origPool, origDaemon, origFacts, origUses, origExists, origKeys
		kubernetes.FootprintPath = origFP
		pollInterval, goneTimeout = origPoll, origGone
		SetHooks(Hooks{})
		readyMu.Lock()
		ready, watching, aptMaintAt, warned = map[string]string{}, map[string]bool{}, map[string]time.Time{}, map[string]bool{}
		readyMu.Unlock()
	})
	return f
}

// watchFor runs the test's server watches under a context of its own; the
// func it returns ends them and waits for them to be gone.
func watchFor(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	watchMu.Lock()
	watchBase = ctx
	watchMu.Unlock()
	return func() {
		cancel()
		watchers.Wait()
		watchMu.Lock()
		watchBase = context.Background()
		watchMu.Unlock()
	}
}

// giveUIDs makes the fake's server-side applies create objects with a UID,
// as an API server does (the fake's tracker leaves it empty).
func giveUIDs(cs *fake.Clientset) {
	var n atomic.Int64
	cs.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.PatchAction)
		if p.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		if _, err := cs.Tracker().Get(p.GetResource(), p.GetNamespace(), p.GetName()); err == nil {
			return false, nil, nil // an update: the default apply keeps the UID
		}
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(p.GetPatch(), nil, nil)
		if err != nil {
			return true, nil, err
		}
		m, _ := apimeta.Accessor(obj)
		m.SetUID(types.UID(fmt.Sprintf("uid-%d", n.Add(1))))
		m.SetNamespace(p.GetNamespace())
		if err := cs.Tracker().Create(p.GetResource(), obj, p.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if m, err := apimeta.Accessor(a.(k8stesting.CreateAction).GetObject()); err == nil && m.GetUID() == "" {
			m.SetUID(types.UID(fmt.Sprintf("uid-%d", n.Add(1))))
		}
		return false, nil, nil
	})
}

func meta(fleetName string) fleetMeta {
	return fleetMeta{Namespace: "fleet", Owner: testOwner, Cluster: testCluster, Fleet: fleetName, Hash: names.FleetHash(testDaemon, testCluster, fleetName)}
}

// names lists the objects of the fake connection, "kind/name", sorted.
func (f *fakePool) objects(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	cms, _ := f.cs.CoreV1().ConfigMaps("fleet").List(ctx, metav1.ListOptions{})
	for _, o := range cms.Items {
		out = append(out, "configmap/"+o.Name)
	}
	svcs, _ := f.cs.CoreV1().Services("fleet").List(ctx, metav1.ListOptions{})
	for _, o := range svcs.Items {
		out = append(out, "svc/"+o.Name)
	}
	nps, _ := f.cs.NetworkingV1().NetworkPolicies("fleet").List(ctx, metav1.ListOptions{})
	for _, o := range nps.Items {
		out = append(out, "netpol/"+o.Name)
	}
	secrets, _ := f.cs.CoreV1().Secrets("fleet").List(ctx, metav1.ListOptions{})
	for _, o := range secrets.Items {
		out = append(out, "secret/"+o.Name)
	}
	deps, _ := f.cs.AppsV1().Deployments("fleet").List(ctx, metav1.ListOptions{})
	for _, o := range deps.Items {
		out = append(out, "deploy/"+o.Name)
	}
	pvcs, _ := f.cs.CoreV1().PersistentVolumeClaims("fleet").List(ctx, metav1.ListOptions{})
	for _, o := range pvcs.Items {
		out = append(out, "pvc/"+o.Name)
	}
	sort.Strings(out)
	return out
}

func want(m fleetMeta, kindSuffixes ...string) []string {
	var out []string
	for _, ks := range kindSuffixes {
		kind, suffix, _ := strings.Cut(ks, "/")
		out = append(out, kind+"/"+m.name(suffix))
	}
	sort.Strings(out)
	return out
}

func sameList(t *testing.T, what string, got, wantList []string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(wantList, " ") {
		t.Fatalf("%s:\n got %v\nwant %v", what, got, wantList)
	}
}

var preparedObjects = []string{"configmap/anchor", "svc/net", "netpol/instances", "netpol/caches", "netpol/egress"}

// TestPrepareFleet: the anchor, the headless Service and the three
// NetworkPolicies, the dependents owned by the anchor; again, nothing
// changes; with the metadata block turned off, the egress policy goes; and
// the fleet is recorded on the connection.
func TestPrepareFleet(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects", f.objects(t), want(m, preparedObjects...))
	anchor, _ := f.cs.CoreV1().ConfigMaps("fleet").Get(context.Background(), m.name(names.SuffixAnchor), metav1.GetOptions{})
	svc, _ := f.cs.CoreV1().Services("fleet").Get(context.Background(), m.name(names.SuffixNet), metav1.GetOptions{})
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].UID != anchor.UID || anchor.UID == "" {
		t.Fatalf("net service owners %v, anchor uid %q", svc.OwnerReferences, anchor.UID)
	}
	fp, _ := kubernetes.LoadFootprint()
	if p := fp.Presence(testCluster, "alpha"); p == nil || p.Hash != m.Hash || p.PreparedAt.IsZero() {
		t.Fatalf("presence %+v", p)
	}

	if err := rt.PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	again, _ := f.cs.CoreV1().ConfigMaps("fleet").Get(context.Background(), m.name(names.SuffixAnchor), metav1.GetOptions{})
	if again.UID != anchor.UID {
		t.Fatal("a second prepare made the anchor again")
	}

	off := false
	f.rec.BlockMetadata = &off
	if err := rt.PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects without the metadata block", f.objects(t), want(m, "configmap/anchor", "svc/net", "netpol/instances", "netpol/caches"))
}

// TestApplyRefusesForeignObjects: an object of a fleet object's name that
// is not this daemon's (another owner, another fleet, no labels) is left
// alone and the prepare fails naming it.
func TestApplyRefusesForeignObjects(t *testing.T) {
	m := meta("alpha")
	for name, labels := range map[string]map[string]string{
		"another daemon": {names.LabelManagedBy: names.ManagedBy, names.LabelOwner: "ffffffffffff", names.LabelCluster: testCluster, names.LabelFleet: m.Hash, names.LabelRole: "anchor"},
		"no labels":      nil,
		"another role":   {names.LabelManagedBy: names.ManagedBy, names.LabelOwner: testOwner, names.LabelCluster: testCluster, names.LabelFleet: m.Hash, names.LabelRole: "net"},
	} {
		t.Run(name, func(t *testing.T) {
			foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: m.name(names.SuffixAnchor), Namespace: "fleet", Labels: labels, UID: "foreign"}}
			f := useFake(t, foreign)
			err := For(testCluster).PrepareFleet("alpha")
			var fe *ForeignError
			if !errors.As(err, &fe) || !strings.Contains(err.Error(), "configmap/"+m.name(names.SuffixAnchor)) {
				t.Fatalf("prepare = %v, want a ForeignError naming the anchor", err)
			}
			sameList(t, "objects", f.objects(t), []string{"configmap/" + m.name(names.SuffixAnchor)})
			got, _ := f.cs.CoreV1().ConfigMaps("fleet").Get(context.Background(), foreign.Name, metav1.GetOptions{})
			if got.UID != "foreign" || got.Labels[names.LabelOwner] != labels[names.LabelOwner] {
				t.Fatalf("the foreign object was changed: %+v", got.ObjectMeta)
			}
		})
	}
}

// TestApplyReplacesADependentOfAnEarlierAnchor: a dependent left from an
// anchor deleted since (whose garbage collection had not come yet) is
// deleted and made again under the current anchor, never adopted.
func TestApplyReplacesADependentOfAnEarlierAnchor(t *testing.T) {
	m := meta("alpha")
	stale := netService(m, anchorRef(m.name(names.SuffixAnchor), "gone-anchor"))
	stale.UID = "stale"
	f := useFake(t, stale)
	if err := For(testCluster).PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	svc, _ := f.cs.CoreV1().Services("fleet").Get(context.Background(), stale.Name, metav1.GetOptions{})
	anchor, _ := f.cs.CoreV1().ConfigMaps("fleet").Get(context.Background(), m.name(names.SuffixAnchor), metav1.GetOptions{})
	if svc.UID == "stale" || len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].UID != anchor.UID {
		t.Fatalf("service uid %q owners %v, anchor %q", svc.UID, svc.OwnerReferences, anchor.UID)
	}
}

// TestEnsureService: a cache's volume, server and Service, after the anchor;
// an ensure does not wait for the server.
func TestEnsureService(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects", f.objects(t), want(m, "configmap/anchor", "pvc/aptcache", "deploy/aptcache", "svc/aptcache"))
	pvc, _ := f.cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), m.name(names.SuffixAptCache), metav1.GetOptions{})
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "10Gi" || len(pvc.OwnerReferences) != 0 {
		t.Fatalf("volume %v owners %v", q.String(), pvc.OwnerReferences)
	}
	if err := rt.EnsureService("alpha", "nope"); err == nil {
		t.Fatal("an unknown service was ensured")
	}
}

// TestEndpoint: a cluster's instances reach the deb and image caches at
// their Services, by the short name their namespace resolves, on the
// Services' ports; checking first that the server answers (Fallback). Shared
// Buildkit is at its Service's TLS port where the connection allows it.
func TestEndpoint(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	for svc, want := range map[fleet.SharedService]struct {
		suffix string
		port   int32
	}{
		fleet.ServiceDebCache:   {names.SuffixAptCache, 3142},
		fleet.ServiceImageCache: {names.SuffixImageCache, 5000},
	} {
		if err := rt.EnsureService("alpha", svc); err != nil {
			t.Fatal(err)
		}
		got, err := rt.Endpoint("alpha", svc)
		if err != nil {
			t.Fatalf("%s: %v", svc, err)
		}
		hostPort := fmt.Sprintf("%s:%d", m.name(want.suffix), want.port)
		if got != (fleet.ServiceEndpoint{URL: "http://" + hostPort, HostPort: hostPort, Fallback: true}) {
			t.Errorf("%s endpoint = %+v, want %s, falling back", svc, got, hostPort)
		}
		// The name and port are the Service's the ensure made.
		s, err := f.cs.CoreV1().Services("fleet").Get(context.Background(), m.name(want.suffix), metav1.GetOptions{})
		if err != nil || len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != want.port {
			t.Errorf("%s Service %v, %v", svc, s, err)
		}
	}
	if other, _ := rt.Endpoint("beta", fleet.ServiceDebCache); !strings.Contains(other.URL, meta("beta").name(names.SuffixAptCache)) {
		t.Errorf("fleet beta's deb cache at %q", other.URL)
	}
	// Shared Buildkit is at its Service's TCP port (mutual TLS, no
	// fallback), unless the connection does not allow it.
	bk := m.name(names.SuffixBuildkit) + ":1234"
	if got, err := rt.Endpoint("alpha", fleet.ServiceBuildkit); err != nil || got != (fleet.ServiceEndpoint{URL: "tcp://" + bk, HostPort: bk}) {
		t.Errorf("buildkit endpoint = %+v, %v", got, err)
	}
	off := false
	f.rec.AllowSharedBuildkit = &off
	if _, err := rt.Endpoint("alpha", fleet.ServiceBuildkit); !errors.Is(err, fleet.ErrNoServiceEndpoint) || !strings.Contains(err.Error(), "allow_shared_buildkit") {
		t.Errorf("buildkit endpoint where not allowed = %v, want ErrNoEndpoint naming the setting", err)
	}
	if _, err := rt.Endpoint("alpha", "nope"); err == nil || errors.Is(err, fleet.ErrNoServiceEndpoint) {
		t.Errorf("unknown service endpoint = %v", err)
	}
	daemonID = func() (string, error) { return "", errors.New("no identity") }
	if _, err := rt.Endpoint("alpha", fleet.ServiceDebCache); err == nil || !strings.Contains(err.Error(), "no identity") {
		t.Errorf("endpoint without the daemon's identity = %v", err)
	}
}

// TestEnsureServiceKeepsAGrownVolume: a volume grown past its setting keeps
// its size, and its StorageClass.
func TestEnsureServiceKeepsAGrownVolume(t *testing.T) {
	m := meta("alpha")
	grown := cacheVolume(m, &fleet.KubeCluster{}, fleet.ServiceDebCache, resource.MustParse("15Gi"))
	grown.Spec.StorageClassName = ptr("csi-hostpath-sc")
	f := useFake(t, grown)
	f.rec.CacheStorageClass = "standard"
	if err := For(testCluster).EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	pvc, _ := f.cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), grown.Name, metav1.GetOptions{})
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "15Gi" || *pvc.Spec.StorageClassName != "csi-hostpath-sc" {
		t.Fatalf("volume %v class %v", q.String(), *pvc.Spec.StorageClassName)
	}
}

// TestStopServiceKeepsTheVolume.
func TestStopServiceKeepsTheVolume(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceImageCache); err != nil {
		t.Fatal(err)
	}
	if err := rt.StopService("alpha", fleet.ServiceImageCache); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects", f.objects(t), want(m, "configmap/anchor", "pvc/imgcache"))
}

// available makes the fake's Deployments report their pod up.
func available(f *fakePool) {
	f.cs.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := f.cs.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), a.GetNamespace(), a.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment).DeepCopy()
		d.Status.AvailableReplicas, d.Status.ObservedGeneration = 1, d.Generation
		return true, d, nil
	})
}

// TestWipeService: the server goes to 0, the volume is deleted and made
// again (a new object), and the server is back before the wipe returns.
func TestWipeService(t *testing.T) {
	f := useFake(t)
	available(f)
	rt := For(testCluster)
	m := meta("alpha")
	// Reactors go in before the server's watch reads the fake.
	var scaled atomic.Bool
	f.cs.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if p := a.(k8stesting.PatchAction); p.GetPatchType() == types.MergePatchType && strings.Contains(string(p.GetPatch()), `"replicas":0`) {
			scaled.Store(true)
		}
		return false, nil, nil
	})
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	before, _ := f.cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), m.name(names.SuffixAptCache), metav1.GetOptions{})
	if err := rt.WipeService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	after, err := f.cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), m.name(names.SuffixAptCache), metav1.GetOptions{})
	if err != nil || after.UID == before.UID || !scaled.Load() {
		t.Fatalf("volume before %q after %q (%v), scaled %v", before.UID, after.UID, err, scaled.Load())
	}
	if st := readyState(testCluster, m, fleet.ServiceDebCache); st != stateReady {
		t.Fatalf("state %q", st)
	}
	// Shared Buildkit is wiped and back the same way, its TLS with it; on a
	// connection that does not run it, a volume left from before only goes.
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	if err := rt.WipeService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects after a buildkit wipe", f.objects(t), want(m, "configmap/anchor", "pvc/aptcache", "deploy/aptcache", "svc/aptcache",
		"pvc/buildkit", "configmap/buildkit", "secret/buildkit", "deploy/buildkit", "svc/buildkit"))
	off := false
	f.rec.AllowSharedBuildkit = &off
	if err := rt.WipeService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	if got := f.objects(t); strings.Contains(strings.Join(got, " "), "pvc/"+m.name(names.SuffixBuildkit)) {
		t.Fatalf("a refused buildkit's volume was made again: %v", got)
	}
}

// TestReleaseFleet: when the fleet's last instance on the connection goes,
// everything of the fleet but its cache volumes is deleted; while a record
// still names the connection, nothing is.
func TestReleaseFleet(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	// Another fleet's objects on the connection are never touched.
	if err := rt.PrepareFleet("beta"); err != nil {
		t.Fatal(err)
	}
	fleetUsesCluster = func(fleetName, cluster string) bool { return fleetName == "alpha" }
	if err := rt.ReleaseFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	if got := f.objects(t); len(got) != 2*len(preparedObjects)+3 {
		t.Fatalf("a fleet still on the connection lost objects: %v", got)
	}
	fleetUsesCluster = func(string, string) bool { return false }
	if err := rt.ReleaseFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	left := append(want(m, "pvc/aptcache"), want(meta("beta"), preparedObjects...)...)
	sort.Strings(left)
	sameList(t, "objects", f.objects(t), left)
	fp, _ := kubernetes.LoadFootprint()
	if p := fp.Presence(testCluster, "alpha"); p == nil || p.PendingTeardown != nil {
		t.Fatalf("presence after a release %+v (kept: the cache volume is there)", p)
	}
}

// TestTeardownFleet: a fleet destroy deletes the fleet's objects, its cache
// volumes too where the connection deletes them with the fleet, and forgets
// the fleet there.
func TestTeardownFleet(t *testing.T) {
	for _, deleteCaches := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete caches %v", deleteCaches), func(t *testing.T) { teardownFleet(t, deleteCaches) })
	}
}

func teardownFleet(t *testing.T, deleteCaches bool) {
	{
		f := useFake(t)
		f.rec.DeleteCacheVolumesOnDestroy = deleteCaches
		rt := For(testCluster)
		m := meta("alpha")
		if err := rt.EnsureService("alpha", fleet.ServiceImageCache); err != nil {
			t.Fatal(err)
		}
		if err := rt.PrepareFleet("alpha"); err != nil {
			t.Fatal(err)
		}
		if err := rt.TeardownFleet("alpha"); err != nil {
			t.Fatal(err)
		}
		var left []string
		if !deleteCaches {
			left = want(m, "pvc/imgcache")
		}
		sameList(t, "objects", f.objects(t), left)
		if fp, _ := kubernetes.LoadFootprint(); fp.Presence(testCluster, "alpha") != nil {
			t.Fatal("a destroyed fleet is still recorded on the connection")
		}
	}
}

// TestTeardownWaitsForTheCluster: a teardown the connection cannot take now
// (not usable, or the owner check refuses) is recorded and comes back as a
// PendingError; the reconciler's pass finishes it once the connection is
// usable, unless the fleet came back meanwhile.
func TestTeardownWaitsForTheCluster(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	reachable := false
	SetHooks(Hooks{Usable: func(string) bool { return reachable }})
	f.rec.DeleteCacheVolumesOnDestroy = true
	err := rt.TeardownFleet("alpha")
	var pending *PendingError
	if !errors.As(err, &pending) {
		t.Fatalf("teardown = %v, want a PendingError", err)
	}
	if got := f.objects(t); len(got) != len(preparedObjects) {
		t.Fatalf("an unreachable cluster lost objects: %v", got)
	}
	fp, _ := kubernetes.LoadFootprint()
	p := fp.Presence(testCluster, "alpha")
	if p == nil || p.PendingTeardown == nil || !p.PendingTeardown.FleetGone || !p.PendingTeardown.DeleteCaches || p.PendingTeardown.Error == "" {
		t.Fatalf("presence %+v", p)
	}
	if n := PendingTeardowns(testCluster); n != 1 {
		t.Fatalf("pending %d", n)
	}

	reachable = true
	fleetUsesCluster = func(string, string) bool { return false }
	if left := RunPendingTeardowns(context.Background(), testCluster); left != 0 {
		t.Fatalf("still pending %d", left)
	}
	sameList(t, "objects after the pass", f.objects(t), nil)
	if fp, _ := kubernetes.LoadFootprint(); fp.Presence(testCluster, "alpha") != nil {
		t.Fatal("the destroyed fleet is still recorded")
	}

	// An owner check that refuses is a pending teardown too.
	fleetUsesCluster = func(fleetName, _ string) bool { return fleetName == "beta" }
	if err := rt.PrepareFleet("beta"); err != nil {
		t.Fatal(err)
	}
	fleetUsesCluster = func(string, string) bool { return false }
	f.gate = func(context.Context) error { return kubernetes.ErrOwnerConflict }
	if err := rt.ReleaseFleet("beta"); !errors.As(err, &pending) || !errors.Is(err, kubernetes.ErrOwnerConflict) {
		t.Fatalf("release under another owner = %v", err)
	}
	// The fleet comes back before the pass: the pass forgets the teardown
	// and deletes nothing.
	f.gate = nil
	fleetUsesCluster = func(fleetName, _ string) bool { return fleetName == "beta" }
	if left := RunPendingTeardowns(context.Background(), testCluster); left != 0 {
		t.Fatalf("still pending %d", left)
	}
	if got := f.objects(t); len(got) != len(preparedObjects) {
		t.Fatalf("a fleet that came back lost objects: %v", got)
	}
	if fp, _ := kubernetes.LoadFootprint(); fp.Presence(testCluster, "beta").PendingTeardown != nil {
		t.Fatal("the void teardown is still owed")
	}
	// A prepare voids a teardown owed too.
	if err := kubernetes.RecordPendingTeardown(testCluster, "beta", meta("beta").Hash, kubernetes.PendingTeardown{}); err != nil {
		t.Fatal(err)
	}
	if err := rt.PrepareFleet("beta"); err != nil {
		t.Fatal(err)
	}
	if PendingTeardowns(testCluster) != 0 {
		t.Fatal("a prepare left the teardown owed")
	}
	if got := PresentClusters("beta"); len(got) != 1 || got[0] != testCluster {
		t.Fatalf("present clusters %v", got)
	}
	_ = m
}

// TestNothingIsMadeForAFleetThatLeftTheCluster: a prepare or an ensure for
// a fleet no record names on the connection (a TUI-connect reconcile that
// read the state before the fleet's last instance there was destroyed and
// its objects released) makes nothing and records nothing.
func TestNothingIsMadeForAFleetThatLeftTheCluster(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	fleetUsesCluster = func(string, string) bool { return false }
	if err := rt.PrepareFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects", f.objects(t), nil)
	if got := PresentClusters("alpha"); len(got) != 0 {
		t.Fatalf("a fleet with no instance there is recorded on %v", got)
	}
}

// TestTeardownOfAConnectionThatDoesNotLoad: a fleet destroy whose
// connection's client does not load now is owed with the cache volumes'
// deletion its record asks for, and the pass deletes them.
func TestTeardownOfAConnectionThatDoesNotLoad(t *testing.T) {
	f := useFake(t)
	f.rec.DeleteCacheVolumesOnDestroy = true
	rt := For(testCluster)
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	f.scopeErr = errors.New(`context "gone" not found in the kubeconfig`)
	var pending *PendingError
	if err := rt.TeardownFleet("alpha"); !errors.As(err, &pending) {
		t.Fatalf("teardown = %v, want a PendingError", err)
	}
	fp, _ := kubernetes.LoadFootprint()
	if p := fp.Presence(testCluster, "alpha"); p == nil || p.PendingTeardown == nil || !p.PendingTeardown.FleetGone || !p.PendingTeardown.DeleteCaches {
		t.Fatalf("presence %+v", p)
	}
	f.scopeErr = nil
	fleetUsesCluster = func(string, string) bool { return false }
	if left := RunPendingTeardowns(context.Background(), testCluster); left != 0 {
		t.Fatalf("still pending %d", left)
	}
	sameList(t, "objects after the pass", f.objects(t), nil)
}

// TestTeardownOfARemovedConnection: nothing can reach what is left there,
// so the fleet is forgotten on it without an error.
func TestTeardownOfARemovedConnection(t *testing.T) {
	useFake(t)
	if err := kubernetes.RecordFleetPrepared("gone-cluster", "alpha", "aaaaaaaa", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := For("gone-cluster").TeardownFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	if got := PresentClusters("alpha"); len(got) != 0 {
		t.Fatalf("present clusters %v", got)
	}
}

// TestCacheUsageAndUpkeep: a volume's use (du in its server's pod) against
// its size; at 90% one whose class grows is grown by half, one whose class
// cannot carries the warning; the deb cache's expiry runs once a day.
func TestCacheUsageAndUpkeep(t *testing.T) {
	f := useFake(t)
	available(f)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "apt-pod", Namespace: "fleet", Labels: m.serviceLabels(fleet.ServiceDebCache)},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := f.cs.CoreV1().Pods("fleet").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	used := int64(9.5 * float64(1<<30) / 1024) // KiB: 95% of 10Gi
	f.exec = func(argv []string) (string, int) {
		if argv[0] == "du" {
			return strings.TrimSpace(resource.NewQuantity(used, resource.DecimalSI).String()) + "\t/var/cache/apt-cacher-ng\n", 0
		}
		return "", 0
	}
	setReady(testCluster, m, fleet.ServiceDebCache, stateReady)

	usage, err := rt.CacheUsage(context.Background(), "alpha")
	if err != nil || len(usage) != 1 {
		t.Fatalf("usage %+v %v", usage, err)
	}
	u := usage[0]
	if u.Service != fleet.ServiceDebCache || u.Location != "minikube" || u.CapacityBytes != 10<<30 || u.Percent() != 95 || u.Expandable ||
		!strings.Contains(u.Warning, "95% full; its StorageClass cannot grow it") || u.State != stateReady {
		t.Fatalf("usage %+v", u)
	}

	// The class grows its volumes: the upkeep grows it by half, once.
	connectionFacts = func(context.Context, string) (kubernetes.Facts, error) {
		return kubernetes.Facts{Capabilities: &fleetgrpc.KubeCapabilities{StorageClasses: []*fleetgrpc.KubeStorageClass{{Name: "standard", IsDefault: true, AllowExpansion: true}}}}, nil
	}
	if err := rt.Maintain(context.Background(), "alpha", []fleet.SharedService{fleet.ServiceDebCache}); err != nil {
		t.Fatal(err)
	}
	pvc, _ := f.cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), m.name(names.SuffixAptCache), metav1.GetOptions{})
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "15Gi" {
		t.Fatalf("grown to %v", q.String())
	}
	if n := f.execCount("sh -c if [ -x /etc/cron.daily/apt-cacher-ng ]"); n != 1 {
		t.Fatalf("apt expiry runs %d", n)
	}
	// Again: the growth asked for has not happened yet (capacity is still
	// 10Gi), and the expiry is not due.
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	if _, err := f.cs.CoreV1().PersistentVolumeClaims("fleet").UpdateStatus(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Maintain(context.Background(), "alpha", []fleet.SharedService{fleet.ServiceDebCache}); err != nil {
		t.Fatal(err)
	}
	pvc, _ = f.cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), m.name(names.SuffixAptCache), metav1.GetOptions{})
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "15Gi" {
		t.Fatalf("grown twice: %v", q.String())
	}
	if n := f.execCount("sh -c"); n != 1 {
		t.Fatalf("apt expiry runs %d, want one a day", n)
	}

	// Auto-expand off: the warning names it.
	no := false
	f.rec.AutoExpandDisk = &no
	usage, _ = rt.CacheUsage(context.Background(), "alpha")
	if len(usage) != 1 || !strings.Contains(usage[0].Warning, "auto-expand is off") {
		t.Fatalf("usage %+v", usage)
	}
}

// TestCacheUsageStateIsTheServers: the state a usage report gives is the
// server's as the cluster shows it now, not what was known of it earlier
// (a server once up may have been deleted, or replaced by one still
// starting); while it is not up, a reason the watch found is kept.
func TestCacheUsageStateIsTheServers(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceDebCache); err != nil {
		t.Fatal(err)
	}
	state := func() string {
		t.Helper()
		usage, err := rt.CacheUsage(context.Background(), "alpha")
		if err != nil || len(usage) != 1 {
			t.Fatalf("usage %+v %v", usage, err)
		}
		return usage[0].State
	}
	setReady(testCluster, m, fleet.ServiceDebCache, stateReady)
	if got := state(); got != stateStarting {
		t.Fatalf("a server with no pod up reads %q", got)
	}
	setReady(testCluster, m, fleet.ServiceDebCache, "failed: its image does not pull: not found")
	if got := state(); got != "failed: its image does not pull: not found" {
		t.Fatalf("the watch's reason is lost: %q", got)
	}
	if err := f.cs.AppsV1().Deployments("fleet").Delete(context.Background(), m.name(names.SuffixAptCache), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "stopped" {
		t.Fatalf("a deleted server reads %q", got)
	}
}

func TestParseDu(t *testing.T) {
	if n, err := parseDu("12\t/x\n"); err != nil || n != 12*1024 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"", "x /y"} {
		if _, err := parseDu(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestRuntimeAnswers: a connection runtime is keyed by its connection, keeps
// its fleets' objects across a stop, and is reachable as the daemon judges
// the connection.
func TestRuntimeAnswers(t *testing.T) {
	useFake(t)
	rt := For(testCluster)
	if rt.Key() != "kubernetes:"+testCluster || rt.ReconfigureOnStart() || rt.ReconcileBudget() <= 0 || rt.InstanceMounts("alpha", fleet.ServiceBuildkit) != nil {
		t.Fatalf("answers %q %v %v", rt.Key(), rt.ReconfigureOnStart(), rt.ReconcileBudget())
	}
	if err := rt.ConnectInstance("alpha", "k8s:x/y/z@g1"); err != nil {
		t.Fatal(err)
	}
	if !rt.Reachable(context.Background()) {
		t.Fatal("unreachable without a hook")
	}
	SetHooks(Hooks{Usable: func(id string) bool { return id != testCluster }})
	if rt.Reachable(context.Background()) {
		t.Fatal("reachable while the daemon judges it unusable")
	}
}

// TestPurgeConnection: a connection being removed takes the objects of the
// fleets recorded on it, cache volumes included, but none of a fleet a
// record still names there.
func TestPurgeConnection(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	for _, name := range []string{"alpha", "beta"} {
		if err := rt.EnsureService(name, fleet.ServiceDebCache); err != nil {
			t.Fatal(err)
		}
		if err := rt.PrepareFleet(name); err != nil {
			t.Fatal(err)
		}
	}
	fleetUsesCluster = func(fleetName, _ string) bool { return fleetName == "beta" }
	if failed := PurgeConnection(context.Background(), testCluster); len(failed) != 0 {
		t.Fatalf("failed %v", failed)
	}
	beta := want(meta("beta"), append(append([]string(nil), preparedObjects...), "pvc/aptcache", "deploy/aptcache", "svc/aptcache")...)
	sameList(t, "objects", f.objects(t), beta)
	if got := PresentClusters("alpha"); len(got) != 0 {
		t.Fatalf("alpha still recorded on %v", got)
	}
	if got := PresentClusters("beta"); len(got) != 1 {
		t.Fatalf("beta recorded on %v", got)
	}
}
