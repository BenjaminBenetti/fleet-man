//go:build !nok8s

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// kube_live_test.go runs the connection RPCs against a real cluster:
//
//	FLEET_KUBE_TEST_CONTEXT=fleet-k8s go test -count=1 -run Live ./internal/server
//
// with the context in ~/.kube/config (or $KUBECONFIG). It creates and
// deletes namespaces named fleet-it-p1-02-server-kube-rpcs-remote-*.

const liveNamespacePrefix = "fleet-it-p1-02-server-kube-rpcs-remote-"

func liveKube(t *testing.T) (kubeconfig, context string) {
	t.Helper()
	context = os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if context == "" {
		t.Skip("FLEET_KUBE_TEST_CONTEXT is not set")
	}
	paths := kubeexec.DefaultKubeconfigPaths(os.Environ())
	return paths[0], context
}

// liveNamespace creates a namespace for the test (deleted after it).
func liveNamespace(t *testing.T, cs kubernetes.Interface, suffix string, labels map[string]string) string {
	t.Helper()
	name := liveNamespacePrefix + suffix
	ctx := context.Background()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	return name
}

// The phase's exit check on a real cluster: minikube connects READY with its
// capabilities; a baseline-labelled namespace is LIMITED; two records on one
// namespace stay isolated; a restored daemon id shows OWNER_CONFLICT; and a
// removed connection's Lease goes with it.
func TestLiveKubeConnections(t *testing.T) {
	kubeconfig, kctx := liveKube(t)
	isolateFleetDir(t)
	svc := newService()
	m := svc.kube
	m.environ = func() []string { return append(os.Environ(), "KUBECONFIG="+kubeconfig) }
	m.loadIdentity = func() (owner.Identity, error) { return testIdentity, nil }

	link, err := connectKube(fleet.KubeCluster{Context: kctx, KubeconfigPaths: []string{kubeconfig}, Namespace: "default"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	plain := liveNamespace(t, link.client, "plain", nil)
	baseline := liveNamespace(t, link.client, "baseline", map[string]string{"pod-security.kubernetes.io/enforce": "baseline"})
	ctx := context.Background()
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go svc.hub.run(runCtx)
	go m.run(runCtx)

	add := func(c *fleetgrpc.KubeCluster) string {
		t.Helper()
		reply, err := svc.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: c})
		if err != nil {
			t.Fatalf("AddKubeCluster(%v): %v", c, err)
		}
		return reply.GetCluster().GetCluster().GetId()
	}
	test := func(id string) *fleetgrpc.KubeClusterStatus {
		t.Helper()
		st, err := m.test(ctx, id, fullRun{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	ready := add(&fleetgrpc.KubeCluster{Context: kctx, Namespace: plain, DisplayName: "live-plain"})
	st := test(ready)
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Fatalf("plain namespace = %v %q\n%v", st.GetState(), st.GetMessage(), st.GetChecks())
	}
	caps := st.GetCapabilities()
	if !strings.HasPrefix(caps.GetVersion(), "v1.") || !caps.GetLocalCluster() || !caps.GetPrivilegedAllowed() || caps.GetUser() == "" ||
		!caps.GetAdmission().GetPrivileged() || !caps.GetAdmission().GetDebugCapabilities() || len(caps.GetStorageClasses()) == 0 || len(caps.GetNodeArchs()) == 0 {
		t.Errorf("capabilities = %v", caps)
	}
	t.Logf("live capabilities: %v", caps)
	if rec, _ := m.record(ready); rec.ClusterUID == "" || rec.Server == "" {
		t.Errorf("bookkeeping not recorded: %+v", rec)
	}

	limited := add(&fleetgrpc.KubeCluster{Context: kctx, Namespace: baseline, DisplayName: "live-baseline"})
	st = test(limited)
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED || !strings.Contains(st.GetMessage(), "privileged denied") || !strings.Contains(st.GetMessage(), "debug caps denied") {
		t.Fatalf("baseline namespace = %v %q\n%v", st.GetState(), st.GetMessage(), st.GetChecks())
	}
	if st.GetCapabilities().GetPsaLevel() != "baseline" || st.GetCapabilities().GetBuildkitAdmitted() {
		t.Errorf("baseline capabilities = %v", st.GetCapabilities())
	}

	// A second record on the plain namespace (the same context through a
	// copy of the kubeconfig): its own Lease, and only its own objects.
	data, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "config-copy")
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	twin := add(&fleetgrpc.KubeCluster{Context: kctx, Namespace: plain, KubeconfigPaths: []string{copyPath}, DisplayName: "live-twin"})
	if st := test(twin); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Fatalf("twin = %v %q", st.GetState(), st.GetMessage())
	}
	for _, id := range []string{ready, twin} {
		labels := names.Base(testIdentity.Owner(), id)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "orphan-of-" + id, Labels: labels, CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour))}}
		if _, err := link.client.CoreV1().ConfigMaps(plain).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := link.client.CoordinationV1().Leases(plain).Get(ctx, names.OwnerLease(testIdentity.DaemonID, id), metav1.GetOptions{}); err != nil {
			t.Errorf("%s owner lease: %v", id, err)
		}
	}
	for _, id := range []string{ready, twin} {
		reply, err := svc.PruneKubeCluster(ctx, &fleetgrpc.PruneKubeClusterRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		// Created just now, the ConfigMaps are spared as young; they are
		// still listed, each by its own connection only.
		if len(reply.GetObjects()) != 1 || reply.GetObjects()[0].GetName() != "orphan-of-"+id {
			t.Errorf("prune %s lists %v", id, reply.GetObjects())
		}
	}

	// A restored copy of this daemon id on another machine.
	restored := newService()
	restored.kube.environ = m.environ
	other := testIdentity
	other.Nonce, other.Host = "bbbbbbbbbbbbbbbb", "restored-host"
	restored.kube.loadIdentity = func() (owner.Identity, error) { return other, nil }
	st, err = restored.kube.test(ctx, ready, fullRun{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT {
		t.Errorf("restored daemon id = %v %q, want OWNER_CONFLICT", st.GetState(), st.GetMessage())
	}

	// Removing a connection releases its Lease (in the background).
	if _, err := svc.RemoveKubeCluster(ctx, &fleetgrpc.RemoveKubeClusterRequest{Id: twin}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := link.client.CoordinationV1().Leases(plain).Get(ctx, names.OwnerLease(testIdentity.DaemonID, twin), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the removed connection's Lease is still there: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
