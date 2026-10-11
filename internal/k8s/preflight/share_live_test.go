//go:build !nok8s && linux

package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// TestLiveSharePods runs the deep test's two-pod probe of one RWO volume
// against a real cluster, in a namespace of its own (deleted when done),
// when FLEET_PREFLIGHT_SHARE_LIVE names a context: it expects both pods to
// mount the volume (every class a single-node share uses should), unless
// FLEET_PREFLIGHT_SHARE_COPY=1 says the class cannot (the copy tier).
// FLEET_PREFLIGHT_SHARE_CLASS picks the class ("" the default).
func TestLiveSharePods(t *testing.T) {
	kctx := os.Getenv("FLEET_PREFLIGHT_SHARE_LIVE")
	if kctx == "" {
		t.Skip("set FLEET_PREFLIGHT_SHARE_LIVE=<context> to run the share probe against a cluster")
	}
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p4-03-share-pods-" + hex.EncodeToString(suffix[:])
	rec := fleet.KubeCluster{ID: "live-" + hex.EncodeToString(suffix[:]), Context: kctx, KubeconfigPaths: kubeexec.DefaultKubeconfigPaths(os.Environ()), Namespace: ns,
		SharedStorageClass: os.Getenv("FLEET_PREFLIGHT_SHARE_CLASS")}
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
	tgt := Target{Cluster: rec, Client: cs, Owner: "liveowner123"}
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true}
	r.rootStorage(ctx)
	r.sharePods(ctx)
	got := check(t, r.res, CheckSharePods)
	t.Logf("share_pods: %v; shared_storage: %v", got, check(t, r.res, CheckSharedStorage))
	wantCopy := os.Getenv("FLEET_PREFLIGHT_SHARE_COPY") == "1"
	if wantCopy {
		if got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || r.caps.GetShareMode() != "copy" {
			t.Fatalf("share_pods %v, mode %s; want the copy tier", got, r.caps.GetShareMode())
		}
	} else if got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || r.caps.GetShareMode() != "single-node" || !r.caps.GetMultiPodRwo() {
		t.Fatalf("share_pods %v, mode %s", got, r.caps.GetShareMode())
	}
	// Deleted, if not gone yet (a volume waits out its pods).
	left, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range left.Items {
		if p.DeletionTimestamp == nil {
			t.Errorf("the probe left pod %s", p.Name)
		}
	}
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pvcs.Items {
		if p.DeletionTimestamp == nil {
			t.Errorf("the probe left volume %s", p.Name)
		}
	}
}
