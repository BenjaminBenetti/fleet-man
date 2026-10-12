//go:build !nok8s

package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_namespace_test.go: the namespace a connection is added with, or set
// to, is created when it does not exist (ensureKubeNamespace), and the
// connection is saved whatever the create does.

// forbidNamespaces makes the fake API server refuse namespace verbs.
func forbidNamespaces(f *kubeFixture, api string, verbs ...string) {
	for _, verb := range verbs {
		f.apis[api].PrependReactor(verb, "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("namespaces is forbidden: User \"minikube-user\" cannot "+verb+" resource \"namespaces\""))
		})
	}
}

// The namespace of a new connection is created with the managed-by label
// (or the labels asked for), before its first preflight; one that exists is
// left as it is.
func TestAddKubeClusterCreatesItsNamespace(t *testing.T) {
	f := newKubeFixture(t)
	reply, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet-new"}})
	if err != nil {
		t.Fatal(err)
	}
	if ns := reply.GetNamespace(); ns.GetNamespace() != "fleet-new" || !ns.GetCreated() || ns.GetError() != "" || ns.GetPermissionDenied() {
		t.Fatalf("namespace result = %v", ns)
	}
	got, err := f.apis["minikube"].CoreV1().Namespaces().Get(context.Background(), "fleet-new", metav1.GetOptions{})
	if err != nil || got.Labels[names.LabelManagedBy] != names.ManagedBy || len(got.Labels) != 1 {
		t.Fatalf("created namespace = %v, %v", got, err)
	}
	// The worker's first preflight finds it.
	id := reply.GetCluster().GetCluster().GetId()
	w := f.svc.kube.newWorker(f.stored(id))
	w.runFull(context.Background(), nil)
	if c := kubeCheckNamed(w.status(), "namespace"); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
		t.Errorf("namespace check after the add = %v", c)
	}

	labelled, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{
		Cluster:         &fleetgrpc.KubeCluster{Context: "minikube-alias", Namespace: "fleet-base"},
		NamespaceLabels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"},
	})
	if err != nil || !labelled.GetNamespace().GetCreated() {
		t.Fatalf("labelled add = %v, %v", labelled, err)
	}
	if got, _ := f.apis["minikube"].CoreV1().Namespaces().Get(context.Background(), "fleet-base", metav1.GetOptions{}); got.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Errorf("labels = %v", got.Labels)
	}

	existing, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	if ns := existing.GetNamespace(); ns.GetNamespace() != "team" || ns.GetCreated() || ns.GetError() != "" {
		t.Errorf("an existing namespace = %v", ns)
	}
	if got, _ := f.apis["prod"].CoreV1().Namespaces().Get(context.Background(), "team", metav1.GetOptions{}); len(got.Labels) != 0 {
		t.Errorf("an existing namespace was relabelled: %v", got.Labels)
	}

	// Labels are checked before anything is saved.
	if _, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{
		Cluster: &fleetgrpc.KubeCluster{Context: "minikube", Namespace: "other", DisplayName: "bad-labels"}, NamespaceLabels: map[string]string{"bad key!": "v"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad labels = %v", err)
	}
	if reg, _ := state.LoadKubeClusters(); len(reg.Clusters) != 3 {
		t.Errorf("connections = %d, want 3", len(reg.Clusters))
	}
}

// Without namespaces create the connection is still saved, and the result
// says so with the manifest to apply. A cluster not reached says it could not
// tell; one whose namespaces are not readable says nothing.
func TestAddKubeClusterKeepsTheRecordWhenTheCreateIsRefused(t *testing.T) {
	f := newKubeFixture(t)
	forbidNamespaces(f, "minikube", "create")
	reply, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet-new"}})
	if err != nil {
		t.Fatalf("a refused namespace create must not refuse the add: %v", err)
	}
	ns := reply.GetNamespace()
	want := "apiVersion: v1\nkind: Namespace\nmetadata:\n  labels:\n    app.kubernetes.io/managed-by: fleet\n  name: fleet-new\n"
	if !ns.GetPermissionDenied() || ns.GetCreated() || ns.GetManifest() != want || !strings.Contains(ns.GetError(), "it needs namespaces create") {
		t.Fatalf("refused create = %v", ns)
	}
	if f.stored(reply.GetCluster().GetCluster().GetId()).Namespace != "fleet-new" {
		t.Error("the connection must be saved")
	}

	unreached, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "token-ctx", Namespace: "far"}})
	if err != nil {
		t.Fatal(err)
	}
	if ns := unreached.GetNamespace(); ns.GetCreated() || ns.GetPermissionDenied() || !strings.HasPrefix(ns.GetError(), "could not tell whether it exists") {
		t.Errorf("unreached = %v", ns)
	}

	g := newKubeFixture(t)
	forbidNamespaces(g, "minikube", "get", "create")
	blind, err := g.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube", Namespace: "mine"}})
	if err != nil {
		t.Fatal(err)
	}
	if ns := blind.GetNamespace(); ns.GetCreated() || ns.GetPermissionDenied() || ns.GetError() != "" {
		t.Errorf("unreadable namespaces = %v, want nothing to say", ns)
	}
}

// Setting a connection's namespace creates it; a refusal keeps the change
// and says so. An update that leaves namespace and target alone says nothing.
func TestUpdateKubeClusterCreatesTheNamespaceItSets(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	set := func(field, value string) *fleetgrpc.UpdateKubeClusterReply {
		t.Helper()
		c := &fleetgrpc.KubeCluster{}
		switch field {
		case "namespace":
			c.Namespace = value
		case "disk_size":
			c.DiskSize = value
		}
		reply, err := f.svc.UpdateKubeCluster(context.Background(), &fleetgrpc.UpdateKubeClusterRequest{Id: id, Cluster: c, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{field}}})
		if err != nil {
			t.Fatalf("set %s=%s: %v", field, value, err)
		}
		return reply
	}
	if ns := set("namespace", "team-a").GetNamespace(); !ns.GetCreated() || ns.GetNamespace() != "team-a" {
		t.Fatalf("set namespace = %v", ns)
	}
	if _, err := f.apis["minikube"].CoreV1().Namespaces().Get(context.Background(), "team-a", metav1.GetOptions{}); err != nil {
		t.Fatalf("team-a: %v", err)
	}
	if ns := set("disk_size", "80Gi").GetNamespace(); ns != nil {
		t.Errorf("an update of another field = %v", ns)
	}

	forbidNamespaces(f, "minikube", "create")
	reply := set("namespace", "team-b")
	if ns := reply.GetNamespace(); !ns.GetPermissionDenied() || !strings.Contains(ns.GetManifest(), "name: team-b") {
		t.Fatalf("refused = %v", ns)
	}
	if reply.GetCluster().GetCluster().GetNamespace() != "team-b" || f.stored(id).Namespace != "team-b" {
		t.Error("the namespace change must be saved")
	}
}

// kubeCheckNamed is the named check of a status, or nil.
func kubeCheckNamed(st *fleetgrpc.KubeClusterStatus, name string) *fleetgrpc.KubeCheck {
	for _, c := range st.GetChecks() {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}
