//go:build !nok8s

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// labelled is the metadata of an object this daemon made for connection
// cluster: its base labels plus extra, created age ago.
func labelled(name, cluster string, age time.Duration, extra map[string]string) metav1.ObjectMeta {
	labels := names.Base(testIdentity.Owner(), cluster)
	for k, v := range extra {
		labels[k] = v
	}
	return metav1.ObjectMeta{Name: name, Namespace: "fleet", Labels: labels, CreationTimestamp: metav1.NewTime(time.Now().Add(-age))}
}

func createObjects(t *testing.T, cs *fake.Clientset, objs ...any) {
	t.Helper()
	ctx := context.Background()
	for _, o := range objs {
		var err error
		switch o := o.(type) {
		case *appsv1.StatefulSet:
			_, err = cs.AppsV1().StatefulSets("fleet").Create(ctx, o, metav1.CreateOptions{})
		case *corev1.PersistentVolumeClaim:
			_, err = cs.CoreV1().PersistentVolumeClaims("fleet").Create(ctx, o, metav1.CreateOptions{})
		case *corev1.ConfigMap:
			_, err = cs.CoreV1().ConfigMaps("fleet").Create(ctx, o, metav1.CreateOptions{})
		case *corev1.Pod:
			_, err = cs.CoreV1().Pods("fleet").Create(ctx, o, metav1.CreateOptions{})
		case *corev1.Secret:
			_, err = cs.CoreV1().Secrets("fleet").Create(ctx, o, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A prune lists only its own connection's unaccounted objects, spares what
// it must, and deletes the rest only with apply.
func TestPruneKubeCluster(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	other := f.add(&fleetgrpc.KubeCluster{Context: "minikube-alias", Namespace: "fleet"}).GetCluster().GetId()
	knownKey := names.InstanceKey(testIdentity.DaemonID, "alpha", "kept", time.Unix(1700000000, 0))
	seedKubeInstance(t, "alpha", "kept", id, knownKey)
	alphaHash := names.FleetHash(testIdentity.DaemonID, id, "alpha")
	instance := map[string]string{names.LabelRole: string(names.RoleInstance)}
	cs := f.apis["minikube"]
	createObjects(t, cs,
		// accounted for: an instance this daemon's state knows
		&appsv1.StatefulSet{ObjectMeta: labelled("fi-alpha-kept-"+knownKey, id, 48*time.Hour, map[string]string{names.LabelRole: string(names.RoleInstance), names.LabelKey: knownKey})},
		// unknown, old: would be deleted
		&appsv1.StatefulSet{ObjectMeta: labelled("fi-gone-one-aaaaaaaa", id, 48*time.Hour, map[string]string{names.LabelRole: string(names.RoleInstance), names.LabelKey: "aaaaaaaa"})},
		&corev1.PersistentVolumeClaim{ObjectMeta: labelled("fi-gone-one-aaaaaaaa", id, 48*time.Hour, map[string]string{names.LabelKey: "aaaaaaaa"})},
		// unknown, but young
		&corev1.PersistentVolumeClaim{ObjectMeta: labelled("fi-new-one-bbbbbbbb", id, 10*time.Minute, map[string]string{names.LabelKey: "bbbbbbbb"})},
		// unknown, but a live pod mounts it
		&corev1.PersistentVolumeClaim{ObjectMeta: labelled("fi-busy-one-cccccccc", id, 48*time.Hour, map[string]string{names.LabelKey: "cccccccc"})},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "someones-pod", Namespace: "fleet"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "fi-busy-one-cccccccc"}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		// a fleet that still exists: kept unless named
		&corev1.PersistentVolumeClaim{ObjectMeta: labelled(names.FleetObject("alpha", alphaHash, names.SuffixShare), id, 48*time.Hour, map[string]string{names.LabelFleet: alphaHash})},
		// the other connection's, on the same namespace: never seen
		&appsv1.StatefulSet{ObjectMeta: labelled("fi-theirs-one-dddddddd", other, 48*time.Hour, instance)},
		// the connection's own managed pull Secret: never listed
		&corev1.Secret{ObjectMeta: labelled("fleet-gwpull-"+testIdentity.Owner()+"-"+id, id, 48*time.Hour, map[string]string{names.LabelRole: string(names.RolePullSecret)})},
		// the connection's seed cache (no instance key): never listed
		&corev1.PersistentVolumeClaim{ObjectMeta: labelled(names.SeedCache("abc234def567"), id, 48*time.Hour, map[string]string{names.LabelRole: string(names.RoleSeedCache)})},
		// another daemon's
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "not-ours", Namespace: "fleet", Labels: names.Base("ffffffffffff", id), CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour))}},
	)

	prune := func(apply bool, fleetName string) *fleetgrpc.PruneKubeClusterReply {
		t.Helper()
		reply, err := f.svc.PruneKubeCluster(context.Background(), &fleetgrpc.PruneKubeClusterRequest{Id: id, Apply: apply, Fleet: fleetName})
		if err != nil {
			t.Fatalf("PruneKubeCluster(apply=%v, fleet=%q): %v", apply, fleetName, err)
		}
		return reply
	}
	actions := func(r *fleetgrpc.PruneKubeClusterReply) string {
		var lines []string
		for _, o := range r.GetObjects() {
			lines = append(lines, o.GetKind()+" "+o.GetName()+": "+o.GetAction())
		}
		return strings.Join(lines, "\n")
	}

	dry := prune(false, "")
	want := strings.Join([]string{
		"pvc fi-busy-one-cccccccc: skip (mounted)",
		"pvc fi-gone-one-aaaaaaaa: would delete",
		"pvc fi-new-one-bbbbbbbb: skip (younger than 1h)",
		"pvc " + names.FleetObject("alpha", alphaHash, names.SuffixShare) + ": kept: fleet exists",
		"sts fi-gone-one-aaaaaaaa: would delete",
	}, "\n")
	if got := actions(dry); got != want {
		t.Errorf("dry run:\n%s\nwant:\n%s", got, want)
	}
	if !dry.GetLeaseHeld() || dry.GetOwner() != testIdentity.Owner() || dry.GetDeleted() != 0 {
		t.Errorf("dry run reply = %v", dry)
	}
	if _, err := cs.AppsV1().StatefulSets("fleet").Get(context.Background(), "fi-gone-one-aaaaaaaa", metav1.GetOptions{}); err != nil {
		t.Fatal("a dry run deleted")
	}

	applied := prune(true, "alpha")
	if applied.GetDeleted() != 3 {
		t.Errorf("applied:\n%s", actions(applied))
	}
	for _, gone := range []string{"fi-gone-one-aaaaaaaa", names.FleetObject("alpha", alphaHash, names.SuffixShare)} {
		if _, err := cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), gone, metav1.GetOptions{}); err == nil {
			t.Errorf("pvc %s survived the prune", gone)
		}
	}
	for _, kept := range []string{"fi-busy-one-cccccccc", "fi-new-one-bbbbbbbb"} {
		if _, err := cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), kept, metav1.GetOptions{}); err != nil {
			t.Errorf("pvc %s was pruned", kept)
		}
	}
	if _, err := cs.AppsV1().StatefulSets("fleet").Get(context.Background(), "fi-theirs-one-dddddddd", metav1.GetOptions{}); err != nil {
		t.Error("the other connection's StatefulSet was pruned")
	}
	if _, err := cs.AppsV1().StatefulSets("fleet").Get(context.Background(), "fi-alpha-kept-"+knownKey, metav1.GetOptions{}); err != nil {
		t.Error("a known instance was pruned")
	}

	if _, err := f.svc.PruneKubeCluster(context.Background(), &fleetgrpc.PruneKubeClusterRequest{Id: id, Fleet: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("prune naming a missing fleet = %v", err)
	}
}

// Two connections to one namespace stay apart: each has its own owner
// Lease, and each sees only its own objects.
func TestTwoKubeRecordsOnOneNamespaceStayIsolated(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	a := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	b := f.add(&fleetgrpc.KubeCluster{Context: "minikube-alias", Namespace: "fleet"}).GetCluster().GetId()
	for _, id := range []string{a, b} {
		if st, err := f.svc.kube.test(context.Background(), id, fullRun{}, nil); err != nil || st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
			t.Fatalf("%s: %v %v", id, st.GetState(), err)
		}
	}
	leases, err := f.apis["minikube"].CoordinationV1().Leases("fleet").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(leases.Items) != 2 || leases.Items[0].Name == leases.Items[1].Name {
		t.Fatalf("owner leases = %v (%v), want one per connection", leases.Items, err)
	}
	createObjects(t, f.apis["minikube"],
		&appsv1.StatefulSet{ObjectMeta: labelled("fi-a-one-aaaaaaaa", a, 48*time.Hour, map[string]string{names.LabelRole: string(names.RoleInstance)})},
		&appsv1.StatefulSet{ObjectMeta: labelled("fi-b-one-bbbbbbbb", b, 48*time.Hour, map[string]string{names.LabelRole: string(names.RoleInstance)})},
	)
	for id, want := range map[string]string{a: "fi-a-one-aaaaaaaa", b: "fi-b-one-bbbbbbbb"} {
		reply, err := f.svc.PruneKubeCluster(context.Background(), &fleetgrpc.PruneKubeClusterRequest{Id: id})
		if err != nil || len(reply.GetObjects()) != 1 || reply.GetObjects()[0].GetName() != want {
			t.Errorf("prune %s = %v, %v; want only %s", id, reply.GetObjects(), err, want)
		}
	}
	// The orphan counts in each one's capabilities are its own.
	for _, id := range []string{a, b} {
		res, err := f.svc.kube.test(context.Background(), id, fullRun{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if o := res.GetCapabilities().GetOrphans(); o.GetInstances() != 1 {
			t.Errorf("%s orphans = %v, want its 1 instance", id, o)
		}
	}
}
