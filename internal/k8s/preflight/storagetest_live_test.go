//go:build !nok8s && linux

package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// TestLiveStorageTest runs the multi-node storage test on a real cluster of
// two nodes or more, in a namespace of its own (deleted when done), when
// FLEET_PREFLIGHT_STORAGE_LIVE names its context: Auto tests the classes a
// share could use from any node and picks FLEET_PREFLIGHT_STORAGE_PASS (an
// RWX class, as nfs-csi), whose test pods ran on two nodes, for multi-node
// mode; FLEET_PREFLIGHT_STORAGE_FAIL (as minikube's hostpath, which Auto
// skips as node-local), named to test, does not pass. With
// FLEET_PREFLIGHT_STORAGE_DEEP=1 a deep run then tests it again and reads
// the nodes' inotify limits. Nothing the tests made is left.
func TestLiveStorageTest(t *testing.T) {
	kctx := os.Getenv("FLEET_PREFLIGHT_STORAGE_LIVE")
	pass := os.Getenv("FLEET_PREFLIGHT_STORAGE_PASS")
	if kctx == "" || pass == "" {
		t.Skip("set FLEET_PREFLIGHT_STORAGE_LIVE=<context> and FLEET_PREFLIGHT_STORAGE_PASS=<class> to run the multi-node storage test against a cluster")
	}
	fail := os.Getenv("FLEET_PREFLIGHT_STORAGE_FAIL")
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p4-04a-storage-" + hex.EncodeToString(suffix[:])
	rec := fleet.KubeCluster{ID: "live-" + hex.EncodeToString(suffix[:]), Context: kctx, KubeconfigPaths: kubeexec.DefaultKubeconfigPaths(os.Environ()), Namespace: ns}
	conn, err := kubeexec.Load(rec, kubeexec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })

	cache := OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	tgt := Target{Cluster: rec, Client: cs, Owner: "liveowner123", StorageCache: cache}
	start := time.Now()
	res := Full(ctx, tgt, func(c *fleetgrpc.KubeCheck) {
		if c.GetName() == CheckSharedStorage {
			t.Logf("%s %s %s", c.GetName(), c.GetResult(), c.GetDetail())
		}
	})
	t.Logf("full run %s: state %s %q", time.Since(start).Round(time.Second), res.State, res.Message)
	caps := res.Capabilities
	for _, sc := range caps.GetStorageClasses() {
		t.Logf("class %s (%s): %v", sc.GetName(), sc.GetProvisioner(), sc.GetMultiNodeTest())
	}
	if caps.GetShareMode() != "multi-node" || caps.GetSharedStorageClass() != pass {
		t.Fatalf("Auto gave %q in %q, want multi-node mode in %s: %v", caps.GetShareMode(), caps.GetSharedStorageClass(), pass, check(t, res, CheckSharedStorage))
	}
	var got *StorageTest
	for _, sc := range caps.GetStorageClasses() {
		if sc.GetName() == pass {
			obj, err := cs.StorageV1().StorageClasses().Get(ctx, pass, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if t, ok := cache.Lookup(StorageTestKey(obj, pass, true), time.Now()); ok {
				got = &t
			}
		}
	}
	if got == nil || !got.Passed() || len(got.Nodes) != 2 || got.Nodes[0] == got.Nodes[1] {
		t.Fatalf("the cached result of %s: %+v", pass, got)
	}

	if fail != "" {
		tgt.TestStorage = []string{fail}
		res = Full(ctx, tgt, nil)
		for _, sc := range res.Capabilities.GetStorageClasses() {
			if sc.GetName() == fail {
				t.Logf("class %s named to test: %v", fail, sc.GetMultiNodeTest())
				if sc.GetMultiNodeTest().GetResult() == fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || sc.GetMultiNodeTest() == nil {
					t.Fatalf("%s passed the multi-node test", fail)
				}
			}
		}
		if res.Capabilities.GetSharedStorageClass() != pass {
			t.Fatalf("after testing %s Auto picked %q", fail, res.Capabilities.GetSharedStorageClass())
		}
	}

	// The deep test tests the class again (its cache notwithstanding) and
	// reads the nodes' inotify limits.
	if os.Getenv("FLEET_PREFLIGHT_STORAGE_DEEP") == "1" {
		tgt.TestStorage, tgt.Deep = nil, true
		before := time.Now()
		res = Full(ctx, tgt, nil)
		t.Logf("deep run: state %s %q; inotify %d instances, %d watches", res.State, res.Message, res.Capabilities.GetInotifyMaxUserInstances(), res.Capabilities.GetInotifyMaxUserWatches())
		obj, err := cs.StorageV1().StorageClasses().Get(ctx, pass, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		again, ok := cache.Lookup(StorageTestKey(obj, pass, true), time.Now())
		if !ok || !again.At.After(before) || !again.Passed() || res.Capabilities.GetShareMode() != "multi-node" {
			t.Fatalf("after the deep test: %+v %v, mode %q", again, ok, res.Capabilities.GetShareMode())
		}
		if res.Capabilities.GetInotifyMaxUserInstances() <= 0 {
			t.Errorf("the deep test read no inotify limits")
		}
	}

	left, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range left.Items {
		if p.DeletionTimestamp == nil {
			t.Errorf("the test left pod %s", p.Name)
		}
	}
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pvcs.Items {
		if p.DeletionTimestamp == nil {
			t.Errorf("the test left volume %s", p.Name)
		}
	}
}
