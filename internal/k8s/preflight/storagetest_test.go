//go:build !nok8s

package preflight

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func params(kv ...string) func(*storagev1.StorageClass) {
	return func(sc *storagev1.StorageClass) {
		sc.Parameters = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			sc.Parameters[kv[i]] = kv[i+1]
		}
	}
}

// TestStorageTestKey: a result is kept by what decides how the class's
// volumes behave (provisioner, parameters, mount options, one node or
// several), not by its name or the parameters' order.
func TestStorageTestKey(t *testing.T) {
	a := storageClass("nfs-a", "nfs.csi.k8s.io", params("server", "nfs.example", "share", "/x"))
	b := storageClass("nfs-b", "nfs.csi.k8s.io", params("share", "/x", "server", "nfs.example"))
	if StorageTestKey(a, a.Name, true) != StorageTestKey(b, b.Name, true) {
		t.Error("two classes of the same settings have different keys")
	}
	c := storageClass("nfs-a", "nfs.csi.k8s.io", params("server", "nfs.example", "share", "/y"))
	d := a.DeepCopy()
	d.MountOptions = []string{"nfsvers=3"}
	e := storageClass("nfs-a", "cephfs.csi.ceph.com", params("server", "nfs.example", "share", "/x"))
	for name, other := range map[string]string{
		"parameters":    StorageTestKey(c, c.Name, true),
		"mount options": StorageTestKey(d, d.Name, true),
		"provisioner":   StorageTestKey(e, e.Name, true),
		"one node":      StorageTestKey(a, a.Name, false),
		"by name":       StorageTestKey(nil, "nfs-a", true),
	} {
		if other == StorageTestKey(a, a.Name, true) {
			t.Errorf("a change of %s keeps the key", name)
		}
	}
	if StorageTestKey(nil, "x", true) != StorageTestKey(nil, "x", true) || StorageTestKey(nil, "x", true) == StorageTestKey(nil, "y", true) {
		t.Error("a class tested by name is keyed by it")
	}
}

// TestStorageCache: results and the two-pod probe's word survive reopening
// the cache; an inconclusive result is due again after its retry time; a
// cache file that is not one starts afresh.
func TestStorageCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kube", "c-1", "storage.json")
	c := OpenStorageCache(path)
	if _, ok := c.Lookup("k", now); ok || c.MultiPodRWO() != nil {
		t.Fatal("a new cache holds something")
	}
	if err := c.Record(StorageTest{Class: "nfs", Key: "k", At: now, MultiNode: true, Mounted: true, Nodes: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Record(StorageTest{Class: "slow", Key: "s", At: now, Inconclusive: true, RetryAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMultiPodRWO(false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file %v %v, want 0600", fi, err)
	}
	c = OpenStorageCache(path)
	if got, ok := c.Lookup("k", now); !ok || !got.Passed() || got.Nodes[1] != "b" {
		t.Fatalf("reopened: %+v %v", got, ok)
	}
	if _, ok := c.Lookup("s", now.Add(30*time.Minute)); !ok {
		t.Error("an inconclusive result is due again before its retry time")
	}
	if _, ok := c.Lookup("s", now.Add(time.Hour)); ok {
		t.Error("an inconclusive result is kept past its retry time")
	}
	if v := c.MultiPodRWO(); v == nil || *v {
		t.Errorf("multi-pod RWO = %v, want false", v)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := OpenStorageCache(path).Lookup("k", now); ok {
		t.Error("a broken cache file still answers")
	}
	var nilCache *StorageCache
	if _, ok := nilCache.Lookup("k", now); ok || nilCache.Record(StorageTest{}) != nil || nilCache.SetMultiPodRWO(true) != nil {
		t.Error("a nil cache is not empty and inert")
	}
}

// stubStorageTests replaces the test runner with results by class (a class
// without one fails the test); it returns the classes it ran, in order.
func stubStorageTests(t *testing.T, results map[string]StorageTest) *[]storageTestRequest {
	t.Helper()
	var mu sync.Mutex
	var ran []storageTestRequest
	old := runStorageTest
	runStorageTest = func(r *run, ctx context.Context, req storageTestRequest) StorageTest {
		mu.Lock()
		ran = append(ran, req)
		mu.Unlock()
		res, ok := results[req.class]
		if !ok {
			t.Errorf("the multi-node test ran on %s", req.class)
		}
		res.Class, res.Key, res.At = req.class, req.key, r.t.now()
		return res
	}
	t.Cleanup(func() { runStorageTest = old })
	return &ran
}

var (
	passed  = StorageTest{MultiNode: true, Mounted: true, Nodes: []string{"a", "b"}, Detail: "pods on a and b shared a ReadWriteMany volume"}
	smb     = StorageTest{MultiNode: true, Mounted: true, Nodes: []string{"a", "b"}, Failed: "chmod", Detail: "file modes do not behave across the pods: modes set on the other node read 777", Fix: "use an NFS Azure Files class"}
	noRWX   = StorageTest{Detail: "no ReadWriteMany volume of the class: access mode not supported"}
	unsure  = StorageTest{Inconclusive: true, Detail: "the test could not tell: the test image did not pull"}
	oneNode = StorageTest{Mounted: true, Nodes: []string{"a", "a"}, Detail: "its files behaved on the one node"}
)

// TestAutoSharedStorage: Auto tests the classes a share could use from any
// node on a cluster of several (not block-only, not node-local ones) and
// holds shares in multi-node mode in one that passed, preferring the
// default; with none, single-node mode in the instance disks' class,
// LIMITED with what each found and the cloud's fix. A connection's own
// shared class is tested alone: one that fails the file system checks is
// refused, one that passes gives multi-node mode, one that does not mount
// on two nodes single-node mode.
func TestAutoSharedStorage(t *testing.T) {
	two := []runtime.Object{cloudNode("a"), cloudNode("b")}
	classes := []runtime.Object{
		storageClass("standard-rwo", "pd.csi.storage.gke.io", isDefault, wffc, expandable),
		storageClass("nfs", "nfs.csi.k8s.io", expandable),
		storageClass("cephfs", "cephfs.csi.ceph.com"),
		storageClass("local", "rancher.io/local-path", wffc),
	}
	for _, tc := range []struct {
		name     string
		rec      fleet.KubeCluster
		results  map[string]StorageTest
		mode     string
		class    string
		result   fleetgrpc.KubeCheckResult
		detail   string
		limited  string
		refused  bool
		tested   []string
		mnResult map[string]fleetgrpc.KubeCheckResult
	}{
		{name: "one passes", results: map[string]StorageTest{"nfs": passed, "cephfs": smb}, mode: "multi-node", class: "nfs",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, detail: "auto → nfs: multi-node mode", tested: []string{"cephfs", "nfs"},
			mnResult: map[string]fleetgrpc.KubeCheckResult{"nfs": fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, "cephfs": fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL,
				"standard-rwo": fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, "local": fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED}},
		{name: "both pass: the expandable one", results: map[string]StorageTest{"nfs": passed, "cephfs": passed}, mode: "multi-node", class: "nfs",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, detail: "auto → nfs", tested: []string{"cephfs", "nfs"}},
		{name: "none passes", results: map[string]StorageTest{"nfs": noRWX, "cephfs": unsure}, mode: "single-node", class: "standard-rwo",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, detail: "no class passed the multi-node test · failed: nfs · could not tell: cephfs", limited: LimitedOneNode, tested: []string{"cephfs", "nfs"},
			mnResult: map[string]fleetgrpc.KubeCheckResult{"nfs": fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, "cephfs": fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN}},
		{name: "own class passes", rec: fleet.KubeCluster{SharedStorageClass: "cephfs"}, results: map[string]StorageTest{"cephfs": passed}, mode: "multi-node", class: "cephfs",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, detail: "multi-node mode", tested: []string{"cephfs"}},
		{name: "own class refused", rec: fleet.KubeCluster{SharedStorageClass: "cephfs"}, results: map[string]StorageTest{"cephfs": smb}, mode: "",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, detail: "storage class cephfs fails the multi-node test: file modes", limited: "fleet shares unavailable", refused: true, tested: []string{"cephfs"}},
		{name: "own class on one node at a time", rec: fleet.KubeCluster{SharedStorageClass: "nfs"}, results: map[string]StorageTest{"nfs": noRWX}, mode: "single-node", class: "nfs",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, detail: "nfs does not give multi-node mode: no ReadWriteMany volume", limited: LimitedOneNode, tested: []string{"nfs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := stubStorageTests(t, tc.results)
			c := newCluster(t, append(append([]runtime.Object{}, two...), classes...)...)
			tgt := target(c, tc.rec)
			tgt.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
			res := Full(context.Background(), tgt, nil)
			got := check(t, res, CheckSharedStorage)
			caps := res.Capabilities
			if got.GetResult() != tc.result || !strings.Contains(got.GetDetail(), tc.detail) {
				t.Fatalf("shared_storage %v", got)
			}
			if caps.GetShareMode() != tc.mode || caps.GetSharedStorageClass() != tc.class || (caps.GetShareRefused() != "") != tc.refused {
				t.Fatalf("mode %q class %q refused %q", caps.GetShareMode(), caps.GetSharedStorageClass(), caps.GetShareRefused())
			}
			var names []string
			for _, r := range *ran {
				names = append(names, r.class)
			}
			if strings.Join(sortedCopy(names), ",") != strings.Join(tc.tested, ",") {
				t.Fatalf("tested %v, want %v", names, tc.tested)
			}
			if tc.limited != "" {
				wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, tc.limited)
			} else if strings.Contains(res.Message, LimitedOneNode) {
				t.Fatalf("limited by single-node mode: %q", res.Message)
			}
			for name, want := range tc.mnResult {
				for _, sc := range caps.GetStorageClasses() {
					if sc.GetName() == name && sc.GetMultiNodeTest().GetResult() != want {
						t.Errorf("%s multi_node_test %v, want %v", name, sc.GetMultiNodeTest(), want)
					}
				}
			}
			if tc.limited == LimitedOneNode && tc.rec.SharedStorageClass == "" && !strings.Contains(got.GetFix(), "Filestore") {
				t.Errorf("no class passed on GKE, and the fix is %q", got.GetFix())
			}
		})
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestStorageTestsRunOnceAClass: a class is tested once, and its cached
// result serves the next runs; a class whose parameters change is tested
// again, as is one named to test now. A cluster of one node and an unsaved
// connection test nothing.
func TestStorageTestsRunOnceAClass(t *testing.T) {
	results := map[string]StorageTest{"nfs": passed}
	ran := stubStorageTests(t, results)
	c := newCluster(t, cloudNode("a"), cloudNode("b"), storageClass("nfs", "nfs.csi.k8s.io", params("share", "/x")), storageClass("pd", "pd.csi.storage.gke.io", isDefault, wffc))
	cache := OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	tgt := target(c, fleet.KubeCluster{})
	tgt.StorageCache = cache
	for i := 0; i < 2; i++ {
		res := Full(context.Background(), tgt, nil)
		if res.Capabilities.GetShareMode() != "multi-node" {
			t.Fatalf("run %d: mode %q", i, res.Capabilities.GetShareMode())
		}
	}
	if len(*ran) != 1 {
		t.Fatalf("%d tests in two runs, want one", len(*ran))
	}
	sc, _ := c.cs.StorageV1().StorageClasses().Get(context.Background(), "nfs", metav1.GetOptions{})
	sc.Parameters["share"] = "/y"
	if _, err := c.cs.StorageV1().StorageClasses().Update(context.Background(), sc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	Full(context.Background(), tgt, nil)
	if len(*ran) != 2 {
		t.Fatalf("a changed class was not tested again (%d tests)", len(*ran))
	}
	tgt.TestStorage = []string{"nfs", "pd"}
	results["pd"] = noRWX
	*ran = nil
	Full(context.Background(), tgt, nil)
	if len(*ran) != 2 {
		t.Fatalf("classes named to test now: %d tests, want both", len(*ran))
	}

	*ran = nil
	one := newCluster(t, minikubeNode(), storageClass("nfs", "nfs.csi.k8s.io"))
	tone := target(one, fleet.KubeCluster{})
	tone.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	res := Full(context.Background(), tone, nil)
	if len(*ran) != 0 || res.Capabilities.GetShareMode() != "single-node" {
		t.Fatalf("a cluster of one node: %d tests, mode %q", len(*ran), res.Capabilities.GetShareMode())
	}
	draft := target(c, fleet.KubeCluster{})
	draft.Cluster.ID = ""
	draft.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	if Full(context.Background(), draft, nil); len(*ran) != 0 {
		t.Fatalf("an unsaved connection ran %d tests", len(*ran))
	}
}

// TestLargeMinimumAsksFirst: Auto does not test a Filestore class (each
// volume at least 1Ti) until it is named to test, the consent; then its
// test volume is that size.
func TestLargeMinimumAsksFirst(t *testing.T) {
	ran := stubStorageTests(t, map[string]StorageTest{"filestore": passed})
	c := newCluster(t, cloudNode("a"), cloudNode("b"), storageClass("filestore", "filestore.csi.storage.gke.io"), storageClass("pd", "pd.csi.storage.gke.io", isDefault, wffc))
	tgt := target(c, fleet.KubeCluster{})
	tgt.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	res := Full(context.Background(), tgt, nil)
	if len(*ran) != 0 {
		t.Fatal("Auto tested a Filestore class without asking")
	}
	for _, sc := range res.Capabilities.GetStorageClasses() {
		if sc.GetName() == "filestore" && (sc.GetMultiNodeTest().GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED || !strings.Contains(sc.GetMultiNodeTest().GetDetail(), "at least 1Ti")) {
			t.Fatalf("filestore multi_node_test %v", sc.GetMultiNodeTest())
		}
	}
	tgt.TestStorage = []string{"filestore"}
	res = Full(context.Background(), tgt, nil)
	if len(*ran) != 1 || (*ran)[0].size != "1Ti" || res.Capabilities.GetSharedStorageClass() != "filestore" {
		t.Fatalf("named: ran %+v, shared class %q", *ran, res.Capabilities.GetSharedStorageClass())
	}
	// Once tested, its cached result serves Auto without asking again.
	tgt.TestStorage = nil
	*ran = nil
	if res = Full(context.Background(), tgt, nil); len(*ran) != 0 || res.Capabilities.GetShareMode() != "multi-node" {
		t.Fatalf("after its test: ran %d, mode %q", len(*ran), res.Capabilities.GetShareMode())
	}
}

// TestDeepTestsStorageAgain: the deep test runs the classes' tests again,
// cached results or not, and judges the share mode by them.
func TestDeepTestsStorageAgain(t *testing.T) {
	results := map[string]StorageTest{"nfs": passed}
	ran := stubStorageTests(t, results)
	c := newCluster(t, cloudNode("a"), cloudNode("b"), storageClass("nfs", "nfs.csi.k8s.io"), storageClass("pd", "pd.csi.storage.gke.io", isDefault, wffc))
	tgt := target(c, fleet.KubeCluster{})
	tgt.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true, multiNode: true, nodesKnown: true}
	r.rootStorage(context.Background())
	r.sharedStorage(context.Background())
	if r.caps.GetShareMode() != "multi-node" || len(*ran) != 1 {
		t.Fatalf("before: mode %q, %d tests", r.caps.GetShareMode(), len(*ran))
	}
	// The class broke since: the deep test finds it, and the same run
	// does not test it twice.
	results["nfs"] = smb
	r.storageFresh = nil
	wait := r.startStorageTests(context.Background())
	wait()
	r.sharedStorage(context.Background())
	if len(*ran) != 2 || r.caps.GetShareMode() != "single-node" {
		t.Fatalf("after the deep test: %d tests, mode %q", len(*ran), r.caps.GetShareMode())
	}
	r.startStorageTests(context.Background())()
	if len(*ran) != 2 {
		t.Fatalf("a class tested in this run was tested again (%d)", len(*ran))
	}
}

// TestStorageTestsShareOneTestsTime: a full run's tests together take at
// most one test's time, so its later checks keep theirs; a class cut short
// has no verdict and is not cached (a later run tests it).
func TestStorageTestsShareOneTestsTime(t *testing.T) {
	old := storageBatchTimeout
	storageBatchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { storageBatchTimeout = old })
	oldRun := runStorageTest
	runStorageTest = func(r *run, ctx context.Context, req storageTestRequest) StorageTest {
		<-ctx.Done()
		return StorageTest{Class: req.class, Key: req.key, At: r.t.now(), Inconclusive: true, Detail: "the test could not tell: cut short"}
	}
	t.Cleanup(func() { runStorageTest = oldRun })
	c := newCluster(t, cloudNode("a"), cloudNode("b"), storageClass("nfs", "nfs.csi.k8s.io"), storageClass("pd", "pd.csi.storage.gke.io", isDefault, wffc))
	tgt := target(c, fleet.KubeCluster{})
	tgt.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true, multiNode: true, nodesKnown: true}
	r.rootStorage(ctx)
	r.sharedStorage(ctx)
	if ctx.Err() != nil {
		t.Fatal("the storage tests spent the run's time")
	}
	if got := r.storage["nfs"]; !got.Inconclusive {
		t.Fatalf("nfs %+v", got)
	}
	if _, ok := tgt.StorageCache.Lookup(r.storage["nfs"].Key, time.Now()); ok {
		t.Fatal("a test cut short was cached")
	}
}

// TestAutoWithoutTheNodes: with the nodes unreadable Auto cannot tell the
// cluster has several, tests nothing and says so on each class it would
// have tested.
func TestAutoWithoutTheNodes(t *testing.T) {
	ctx := context.Background()
	ran := stubStorageTests(t, map[string]StorageTest{})
	c := newCluster(t, storageClass("nfs", "nfs.csi.k8s.io"), storageClass("pd", "pd.csi.storage.gke.io", isDefault, wffc))
	tgt := target(c, fleet.KubeCluster{})
	tgt.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true}
	r.rootStorage(ctx)
	r.sharedStorage(ctx)
	if len(*ran) != 0 || r.caps.GetShareMode() != "single-node" {
		t.Fatalf("nodes unreadable: %d tests, mode %q", len(*ran), r.caps.GetShareMode())
	}
	for _, sc := range r.caps.GetStorageClasses() {
		if sc.GetName() == "nfs" && !strings.Contains(sc.GetMultiNodeTest().GetDetail(), "nodes are not readable") {
			t.Fatalf("nfs multi_node_test %v", sc.GetMultiNodeTest())
		}
	}
}

// storageFake plays the test's pods: each is scheduled on its node and
// runs; their logs are logs's.
type storageFake struct {
	mu      sync.Mutex
	pods    map[string]*corev1.Pod
	pvc     *corev1.PersistentVolumeClaim
	nodes   map[string]string // pod suffix -> node
	waiting map[string]string // pod suffix -> waiting reason (never runs)
	events  []corev1.Event
	deleted []string
}

func (f *storageFake) install(c *cluster) {
	f.pods = map[string]*corev1.Pod{}
	c.cs.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pvc := a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		f.mu.Lock()
		f.pvc = pvc.DeepCopy()
		f.mu.Unlock()
		return true, pvc, nil
	})
	c.cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !strings.HasPrefix(pod.Name, "fleet-probe-mn-") {
			return false, nil, nil
		}
		f.mu.Lock()
		f.pods[pod.Name] = pod.DeepCopy()
		f.mu.Unlock()
		return true, pod, nil
	})
	c.cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		f.mu.Lock()
		defer f.mu.Unlock()
		pod := f.pods[name]
		if pod == nil {
			return false, nil, nil
		}
		out := pod.DeepCopy()
		suffix := name[len(name)-1:]
		if reason := f.waiting[suffix]; reason != "" {
			out.Status.Phase = corev1.PodPending
			out.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}
			return true, out, nil
		}
		out.Spec.NodeName = f.nodes[suffix]
		out.Status.Phase = corev1.PodSucceeded
		return true, out, nil
	})
	c.cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return true, &corev1.EventList{Items: append([]corev1.Event(nil), f.events...)}, nil
	})
	c.cs.PrependReactor("delete", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		if !strings.HasPrefix(name, "fleet-probe-mn-") {
			return false, nil, nil
		}
		f.mu.Lock()
		f.deleted = append(f.deleted, a.GetResource().Resource+"/"+name)
		f.mu.Unlock()
		return true, nil, nil
	})
}

// fakeLogs makes the test pods' logs those of the script's checks.
func fakeLogs(t *testing.T, a, b []storageLine) {
	t.Helper()
	old := clientLog
	clientLog = func(_ *run, _ context.Context, name string) (string, error) {
		lines := a
		if strings.HasSuffix(name, "-b") {
			lines = b
		}
		var sb strings.Builder
		for _, l := range append(lines, storageLine{Check: "done", OK: true}) {
			raw, _ := json.Marshal(l)
			sb.Write(raw)
			sb.WriteString("\n")
		}
		return sb.String(), nil
	}
	t.Cleanup(func() { clientLog = old })
}

func fastStorageTest(t *testing.T) {
	t.Helper()
	old := []time.Duration{storageTestStart, storageTestMount, storageTestRun, storageTestPoll}
	storageTestStart, storageTestMount, storageTestRun, storageTestPoll = 200*time.Millisecond, 200*time.Millisecond, time.Second, time.Millisecond
	t.Cleanup(func() {
		storageTestStart, storageTestMount, storageTestRun, storageTestPoll = old[0], old[1], old[2], old[3]
	})
}

// TestStorageTestPods: the test makes a ReadWriteMany volume of the class
// and two pods on different nodes (a required anti-affinity), root with
// only the owner and mode capabilities where root is admitted, at one
// SELinux level, of the pinned image; it reads their checks, names the
// first that failed with the class's known fix, and deletes all it made.
func TestStorageTestPods(t *testing.T) {
	fastStorageTest(t)
	ok := []storageLine{{Check: "visible", OK: true}, {Check: "chmod", OK: true}}
	for _, tc := range []struct {
		name    string
		a, b    []storageLine
		nodes   map[string]string
		waiting map[string]string
		events  []corev1.Event
		check   func(t *testing.T, res StorageTest)
	}{
		{name: "passes", b: ok, nodes: map[string]string{"a": "node-a", "b": "node-b"}, check: func(t *testing.T, res StorageTest) {
			if !res.Passed() || res.Nodes[0] != "node-a" || res.Nodes[1] != "node-b" {
				t.Fatalf("%+v", res)
			}
		}},
		{name: "fails a check", nodes: map[string]string{"a": "node-a", "b": "node-b"},
			b: []storageLine{{Check: "visible", OK: true}, {Check: "flock", OK: false, Detail: "this pod took a lock"}, {Check: "chmod", OK: false, Detail: "modes read 777"}},
			check: func(t *testing.T, res StorageTest) {
				if !res.Refused() || res.Failed != "chmod" || !strings.Contains(res.Detail, "file modes") || !strings.Contains(res.Fix, "NFS Azure Files") {
					t.Fatalf("%+v", res)
				}
			}},
		{name: "a lock the first pod cannot take", nodes: map[string]string{"a": "node-a", "b": "node-b"},
			a: []storageLine{{Check: "unix_socket", OK: true}, {Check: "flock", OK: false, Detail: "taking a lock on the first node: [Errno 37] No locks available"}},
			b: []storageLine{{Check: "visible", OK: true}, {Check: "append", OK: true}, {Check: "flock", OK: false, Detail: "this pod took a lock the pod on the other node held"}},
			check: func(t *testing.T, res StorageTest) {
				if !res.Refused() || res.Failed != "flock" || !strings.Contains(res.Detail, "No locks available") {
					t.Fatalf("%+v", res)
				}
			}},
		{name: "the pods lose step", nodes: map[string]string{"a": "node-a", "b": "node-b"},
			a: []storageLine{{Check: "unix_socket", OK: true}, {Check: "steps", OK: false, Detail: "the pod on the other node did not reach step 2 within 30s"}},
			b: []storageLine{{Check: "visible", OK: true}, {Check: "steps", OK: false, Detail: "the pod on the other node did not reach step 2 within 30s"}},
			check: func(t *testing.T, res StorageTest) {
				// No check failed: no verdict, tried again later.
				if res.Refused() || !res.Inconclusive || res.Failed != "" || !strings.Contains(res.Detail, "lost step") {
					t.Fatalf("%+v", res)
				}
			}},
		{name: "the first pod cannot write the volume", nodes: map[string]string{"a": "node-a", "b": "node-b"},
			a: []storageLine{{Check: "visible", OK: false, Detail: "on the first node: [Errno 13] Permission denied: 'new.txt'"}},
			b: []storageLine{{Check: "visible", OK: false, Detail: "a file the pod on the other node wrote was not seen here within 30s"}},
			check: func(t *testing.T, res StorageTest) {
				// Not two nodes that never see each other's files: a volume
				// the pods cannot write, refused.
				if !res.Refused() || res.Failed != "visible" || !strings.Contains(res.Detail, "Permission denied") {
					t.Fatalf("%+v", res)
				}
			}},
		{name: "each node its own files", nodes: map[string]string{"a": "node-a", "b": "node-b"},
			b: []storageLine{{Check: "visible", OK: false, Detail: "a file the pod on the other node wrote was not seen here within 30s"}},
			check: func(t *testing.T, res StorageTest) {
				// Not a file system to refuse: one the share holds on a node.
				if res.Mounted || res.Refused() || res.Inconclusive || res.MultiNode && res.Passed() || !strings.Contains(res.Detail, "do not see each other's files") {
					t.Fatalf("%+v", res)
				}
			}},
		{name: "no RWX volume", waiting: map[string]string{"a": "ContainerCreating"}, events: []corev1.Event{{
			ObjectMeta:     metav1.ObjectMeta{Name: "e", Namespace: testNS},
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim"}, Reason: "ProvisioningFailed",
			Message: "failed to provision volume: requested access mode RWX is not supported", LastTimestamp: metav1.NewTime(time.Now().Add(time.Minute)),
		}}, check: func(t *testing.T, res StorageTest) {
			if res.Mounted || res.Inconclusive || res.MultiNode || !strings.Contains(res.Detail, "no ReadWriteMany volume of the class: failed to provision") {
				t.Fatalf("%+v", res)
			}
		}},
		{name: "a provisioning failure that may pass", waiting: map[string]string{"a": "ContainerCreating"}, events: []corev1.Event{{
			ObjectMeta:     metav1.ObjectMeta{Name: "e", Namespace: testNS},
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim"}, Reason: "ProvisioningFailed",
			Message: "failed to provision volume with StorageClass \"azurefile-csi-nfs\": rpc error: code = DeadlineExceeded desc = context deadline exceeded", LastTimestamp: metav1.NewTime(time.Now().Add(time.Minute)),
		}}, check: func(t *testing.T, res StorageTest) {
			// A cloud file share still being made is no refusal of RWX: no
			// verdict, tried again later.
			if !res.Inconclusive || res.Mounted || !strings.Contains(res.Detail, "did not make the test volume") || !strings.Contains(res.Detail, "DeadlineExceeded") {
				t.Fatalf("%+v", res)
			}
		}},
		{name: "second node cannot mount", nodes: map[string]string{"a": "node-a"}, waiting: map[string]string{"b": "ContainerCreating"}, events: []corev1.Event{{
			ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: testNS}, Reason: "FailedAttachVolume",
			Message: "Multi-Attach error for volume pvc-1", LastTimestamp: metav1.Now(),
		}}, check: func(t *testing.T, res StorageTest) {
			if res.Mounted || res.Inconclusive || !strings.Contains(res.Detail, "Multi-Attach error") {
				t.Fatalf("%+v", res)
			}
		}},
		{name: "image does not pull", waiting: map[string]string{"a": "ImagePullBackOff"}, check: func(t *testing.T, res StorageTest) {
			if !res.Inconclusive || res.RetryAt.IsZero() || !strings.Contains(res.Detail, "did not pull") {
				t.Fatalf("%+v", res)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, cloudNode("node-a"), cloudNode("node-b"))
			f := &storageFake{nodes: tc.nodes, waiting: tc.waiting, events: tc.events}
			f.install(c)
			a := tc.a
			if a == nil {
				a = ok
			}
			fakeLogs(t, a, tc.b)
			tgt := target(c, fleet.KubeCluster{})
			r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}, nsExists: true, multiNode: true, nodesKnown: true}
			sc := storageClass("azurefile", "file.csi.azure.com", params("skuName", "Standard_LRS"))
			res := r.storageTest(context.Background(), storageTestRequest{class: "azurefile", sc: sc, key: "k", size: "1Gi"})
			tc.check(t, res)
			if f.pvc == nil || f.pvc.Spec.AccessModes[0] != corev1.ReadWriteMany || *f.pvc.Spec.StorageClassName != "azurefile" {
				t.Fatalf("the test volume %+v", f.pvc)
			}
			for name, pod := range f.pods {
				var anti *corev1.PodAntiAffinity
				if pod.Spec.Affinity != nil {
					anti = pod.Spec.Affinity.PodAntiAffinity
				}
				if anti == nil || len(anti.RequiredDuringSchedulingIgnoredDuringExecution) != 1 || anti.RequiredDuringSchedulingIgnoredDuringExecution[0].TopologyKey != corev1.LabelHostname {
					t.Errorf("%s is not kept off the other's node: %+v", name, pod.Spec.Affinity)
				}
				ctr := pod.Spec.Containers[0]
				if ctr.Image != storageTestImage || *pod.Spec.SecurityContext.RunAsUser != 0 || len(ctr.SecurityContext.Capabilities.Add) != 3 {
					t.Errorf("%s: image %s, uid %d, caps %v", name, ctr.Image, *pod.Spec.SecurityContext.RunAsUser, ctr.SecurityContext.Capabilities)
				}
				if pod.Spec.SecurityContext.SELinuxOptions == nil || pod.Spec.SecurityContext.SELinuxOptions.Level == "" {
					t.Errorf("%s has no SELinux level", name)
				}
				if ctr.VolumeMounts[0].MountPath != "/data" || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != f.pvc.Name {
					t.Errorf("%s does not mount the test volume", name)
				}
			}
			if len(f.deleted) != 3 {
				t.Fatalf("deleted %v, want both pods and the volume", f.deleted)
			}
		})
	}
}

// TestStorageTestPodsWithoutRoot: where root is not admitted the test pods
// run as the share uid, its gid owning the volume.
func TestStorageTestPodsWithoutRoot(t *testing.T) {
	c := newCluster(t)
	tgt := target(c, fleet.KubeCluster{})
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{Admission: &fleetgrpc.KubeAdmission{Unprivileged: false}}}
	pod, err := r.storagePod("p", "g", "a", "claim")
	if err != nil {
		t.Fatal(err)
	}
	psc := pod.Spec.SecurityContext
	if psc.RunAsUser == nil || *psc.RunAsUser == 0 || psc.FSGroup == nil || !*psc.RunAsNonRoot {
		t.Fatalf("without root: %+v", psc)
	}
	// The nodes are not readable: only preferred apart.
	if pod.Spec.Affinity == nil {
		t.Fatal("no affinity")
	}
	if a := pod.Spec.Affinity.PodAntiAffinity; a == nil || len(a.PreferredDuringSchedulingIgnoredDuringExecution) != 1 || len(a.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Fatalf("anti-affinity %+v", pod.Spec.Affinity)
	}
}

// TestStorageScript runs the test pods' script in two processes over one
// folder, as two pods on a node would: every check passes on a local file
// system.
func TestStorageScript(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), "s.py")
	if err := os.WriteFile(script, []byte(storageScript), 0o600); err != nil {
		t.Fatal(err)
	}
	outs := make([][]byte, 2)
	var wg sync.WaitGroup
	for i, role := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			outs[i], _ = exec.CommandContext(ctx, py, script, role, "30", "4321", dir).CombinedOutput()
		}()
	}
	wg.Wait()
	for i, out := range outs {
		lines, err := parseStorageLog(string(out))
		if err != nil {
			t.Fatalf("pod %d: %v\n%s", i, err, out)
		}
		if failed, detail := firstFailure(lines); failed != "" {
			t.Fatalf("pod %d: %s failed: %s\n%s", i, failed, detail, out)
		}
	}
	b, _ := parseStorageLog(string(outs[1]))
	seen := map[string]bool{}
	for _, l := range b {
		seen[l.Check] = true
	}
	for _, want := range []string{"visible", "rename", "symlink", "chmod", "uid", "append", "flock"} {
		if !seen[want] {
			t.Errorf("the second pod never ran %s", want)
		}
	}
}

// TestStorageScriptNamesALockItCannotTake: a lock the first pod cannot
// take (NFS without its lock manager: ENOLCK) fails the lock check, in both
// pods, which still finish their steps; it is not mistaken for the pods not
// seeing each other's files.
func TestStorageScriptNamesALockItCannotTake(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	bin := t.TempDir()
	script := filepath.Join(bin, "s.py")
	wrap := filepath.Join(bin, "wrap.py")
	if err := os.WriteFile(script, []byte(storageScript), 0o600); err != nil {
		t.Fatal(err)
	}
	// The first pod's flock fails as on an NFS mount without lockd.
	const wrapper = `import errno, fcntl, sys
real = fcntl.flock
def flock(f, op):
    if sys.argv[1] == "a":
        raise OSError(errno.ENOLCK, "No locks available")
    return real(f, op)
fcntl.flock = flock
src = open(sys.argv.pop(1)).read()
exec(compile(src, "s.py", "exec"))
`
	if err := os.WriteFile(wrap, []byte(wrapper), 0o600); err != nil {
		t.Fatal(err)
	}
	outs := make([][]byte, 2)
	var wg sync.WaitGroup
	for i, role := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			outs[i], _ = exec.CommandContext(ctx, py, "-I", wrap, script, role, "30", "4321", dir).CombinedOutput()
		}()
	}
	wg.Wait()
	var all []storageLine
	for i, out := range outs {
		lines, err := parseStorageLog(string(out))
		if err != nil {
			t.Fatalf("pod %d: %v\n%s", i, err, out)
		}
		if !strings.Contains(string(out), `"done"`) {
			t.Errorf("pod %d did not finish its steps:\n%s", i, out)
		}
		all = append(all, lines...)
	}
	if failed, detail := firstFailure(all); failed != "flock" || !strings.Contains(detail, "No locks available") {
		t.Fatalf("first failure %s (%s), want flock\n%s\n%s", failed, detail, outs[0], outs[1])
	}
}

// TestParseStorageLog: a log without its done line did not finish (unless a
// check failed, which ends it early); lines that are not checks are left.
func TestParseStorageLog(t *testing.T) {
	if _, err := parseStorageLog(`{"check":"visible","ok":true}` + "\n"); err == nil {
		t.Error("a log without done finished")
	}
	lines, err := parseStorageLog("Traceback...\n" + `{"check":"visible","ok":false,"detail":"x"}` + "\n")
	if err != nil || len(lines) != 1 {
		t.Fatalf("a failed check's log: %v %v", lines, err)
	}
	if f, _ := firstFailure([]storageLine{{Check: "flock"}, {Check: "visible"}}); f != "visible" {
		t.Errorf("first failure %q, want visible (the order of the checks)", f)
	}
}

// TestClassFix names the known remedies.
func TestClassFix(t *testing.T) {
	for name, tc := range map[string]struct {
		sc    *storagev1.StorageClass
		check string
		want  string
	}{
		"smb":           {storageClass("azurefile", "file.csi.azure.com"), "chmod", "NFS Azure Files"},
		"azure nfs ok":  {storageClass("azurefile-nfs", "file.csi.azure.com", params("protocol", "nfs")), "flock", "lock manager"},
		"efs ap":        {storageClass("efs", "efs.csi.aws.com", params("provisioningMode", "efs-ap")), "uid", "statically provisioned EFS"},
		"root squash":   {storageClass("nfs", "nfs.csi.k8s.io"), "chown", "no_root_squash"},
		"block":         {storageClass("gp3", "ebs.csi.aws.com"), "", "block disks"},
		"nothing known": {storageClass("x", "example.com/x"), "rename", ""},
	} {
		if got := classFix(tc.sc, tc.check); (tc.want == "" && got != "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: fix %q, want %q", name, got, tc.want)
		}
	}
	if !strings.Contains(noClassFix([]*fleetgrpc.KubeNode{{Name: "n"}}, map[string]string{"n": "azure:///subscriptions/x"}), "azurefile-csi-nfs") {
		t.Error("AKS: no NFS Azure Files suggestion")
	}
	if !strings.Contains(noClassFix([]*fleetgrpc.KubeNode{{Name: "n"}}, map[string]string{"n": "aws:///eu-west-1a/i-1"}), "EFS") {
		t.Error("EKS: no EFS suggestion")
	}
}
