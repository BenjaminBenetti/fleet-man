//go:build !nok8s && linux

package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// TestLiveNetworkProbe runs the deep test's network probe against a real
// cluster, in a namespace of its own (deleted when done), when
// FLEET_PREFLIGHT_NETWORK_LIVE names a context. With
// FLEET_PREFLIGHT_METADATA_STANDIN=1 (a throwaway cluster only) it first puts
// a stand-in metadata server on the node, 169.254.169.254 on its loopback,
// and then expects the probe to find it unreachable under the metadata block
// and reachable with the block off; without it, it expects the block to hold
// against whatever answers there (nothing off the clouds, the cloud's own
// metadata service on a cloud VM). FLEET_PREFLIGHT_NETWORK_ENFORCED=0 expects
// a network plugin that does not enforce NetworkPolicies.
func TestLiveNetworkProbe(t *testing.T) {
	kctx := os.Getenv("FLEET_PREFLIGHT_NETWORK_LIVE")
	if kctx == "" {
		t.Skip("set FLEET_PREFLIGHT_NETWORK_LIVE=<context> to run the network probe against a cluster")
	}
	enforced := os.Getenv("FLEET_PREFLIGHT_NETWORK_ENFORCED") != "0"
	standIn := os.Getenv("FLEET_PREFLIGHT_METADATA_STANDIN") == "1"
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p5-00-kubert-objects-ensure-wipe-p" + hex.EncodeToString(suffix[:])
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

	if standIn {
		yes, zero := true, int64(0)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "metadata-stand-in"},
			Spec: corev1.PodSpec{
				HostNetwork: true, TerminationGracePeriodSeconds: &zero,
				Containers: []corev1.Container{{Name: "box", Image: probeImage, SecurityContext: &corev1.SecurityContext{Privileged: &yes},
					Command: []string{"sh", "-c", "ip addr add 169.254.169.254/32 dev lo 2>/dev/null || true; mkdir -p /tmp/www && echo stand-in > /tmp/www/index.html && exec httpd -f -p 169.254.169.254:80 -h /tmp/www"}}},
			},
		}
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			p, err := cs.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
			if err == nil && p.Status.Phase == corev1.PodRunning {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the stand-in never ran")
			}
			time.Sleep(time.Second)
		}
		time.Sleep(2 * time.Second)
	}

	probe := func(block bool) *run {
		t.Helper()
		r := rec
		r.BlockMetadata = &block
		tgt := Target{Cluster: r, Client: cs, Owner: "0123456789ab", Now: time.Now}
		run := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true}
		start := time.Now()
		run.networkProbe(ctx)
		for _, c := range run.res.Checks {
			t.Logf("block=%v %s %s: %s (%v)", block, c.GetName(), c.GetResult(), c.GetDetail(), time.Since(start).Round(time.Second))
		}
		for _, w := range run.res.Warnings {
			t.Logf("block=%v warning: %s", block, w)
		}
		if got := run.caps.GetNetworkPolicyEnforced(); got != enforced {
			t.Errorf("block=%v: network policies enforced = %v, want %v", block, got, enforced)
		}
		return run
	}
	on := probe(true)
	if on.caps.GetMetadataReachable() == enforced && enforced {
		t.Errorf("under the metadata block a pod reached 169.254.169.254")
	}
	if standIn {
		off := probe(false)
		if !off.caps.GetMetadataReachable() {
			t.Errorf("with the block off a pod did not reach the stand-in metadata server")
		}
		if !strings.Contains(strings.Join(off.res.Warnings, "\n"), "169.254.169.254 reachable from pods (egress block off)") {
			t.Errorf("warnings %q", off.res.Warnings)
		}
	}
	// Nothing of the probe's is left.
	pods, _ := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelProbe})
	nps, _ := cs.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 || len(nps.Items) != 0 {
		t.Errorf("the probe left %d pods and %d policies", len(pods.Items), len(nps.Items))
	}
}
