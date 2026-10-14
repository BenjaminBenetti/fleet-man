//go:build !nok8s

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestLiveKubeReconcile runs the reconciler against a real cluster
// (FLEET_KUBE_TEST_CONTEXT; a fleet-it-p2-11-reconcile-* namespace deleted
// when done), with the real pool, the real owner Lease and the connection's
// real worker: once the worker judges the connection usable, a destroyed
// key's objects and a dead job's journal objects are deleted, and an
// interrupted rebuild (before its reset) is undone, its record back to the
// status it had.
func TestLiveKubeReconcile(t *testing.T) {
	kubeconfig, kctx := liveKube(t)
	isolateFleetDir(t)
	env := append(os.Environ(), "KUBECONFIG="+kubeconfig)
	svc := newService()
	m := svc.kube
	m.environ = func() []string { return env }
	m.loadIdentity = func() (owner.Identity, error) { return testIdentity, nil }
	pool := kubernetes.NewPool(kubernetes.PoolOptions{
		Environ: func() []string { return env },
		Owner:   func() (string, error) { return testIdentity.Owner(), nil },
	})
	t.Cleanup(pool.Close)
	svc.kubeRecovery.pool = func() kubeRecoveryPool { return pool }

	link, err := connectKube(fleet.KubeCluster{Context: kctx, KubeconfigPaths: []string{kubeconfig}, Namespace: "default"}, env)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p2-11-reconcile-" + hex.EncodeToString(suffix[:])
	ctx := context.Background()
	cs := link.client
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	reply, err := svc.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: kctx, Namespace: ns, KubeconfigPaths: []string{kubeconfig}}})
	if err != nil {
		t.Fatal(err)
	}
	id := reply.GetCluster().GetCluster().GetId()

	meta := func(name, key, mark string, role names.Role) metav1.ObjectMeta {
		labels := names.Base(testIdentity.Owner(), id)
		labels[names.LabelKey] = key
		if role != "" {
			labels[names.LabelRole] = string(role)
		}
		m := metav1.ObjectMeta{Name: name, Labels: labels}
		if mark != "" {
			m.Annotations = map[string]string{names.AnnotationOp: mark}
		}
		return m
	}
	zero := int32(0)
	stsOf := func(name, key, mark string) *appsv1.StatefulSet {
		m := meta(name, key, mark, names.RoleInstance)
		return &appsv1.StatefulSet{ObjectMeta: m, Spec: appsv1.StatefulSetSpec{
			Replicas: &zero, ServiceName: name, Selector: &metav1.LabelSelector{MatchLabels: m.Labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: m.Labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "mcr.microsoft.com/devcontainers/base:debian-12"}}}},
		}}
	}
	// A destroyed instance's leftovers.
	if _, err := cs.AppsV1().StatefulSets(ns).Create(ctx, stsOf("fi-alpha-dead-deadaaaa", "deadaaaa", ""), metav1.CreateOptions{FieldManager: kubernetes.DefaultFieldManager}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: meta("fi-alpha-dead-deadaaaa", "deadaaaa", "", ""),
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Mi")}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := kubernetes.AddTombstone(kubernetes.Tombstone{Cluster: id, Key: "deadaaaa", Fleet: "alpha", Instance: "dead"}); err != nil {
		t.Fatal(err)
	}
	// A create whose record is gone, its journal still current.
	sec, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: meta("fi-alpha-lost-lostaaaa-spec-g1", "lostaaaa", "create@seed", "")}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kubernetes.BeginJournal(kubernetes.Journal{Op: kubernetes.OpCreate, Fleet: "alpha", Instance: "lost", Cluster: id, Namespace: ns, Key: "lostaaaa", Gen: 1, Step: "seed",
		Objects: []kubernetes.JournalObject{{Kind: kubernetes.KindSecret, Name: sec.Name, UID: string(sec.UID), Created: true, Anchor: true}}}); err != nil {
		t.Fatal(err)
	}
	// A rebuild interrupted before its reset.
	set, err := cs.AppsV1().StatefulSets(ns).Create(ctx, stsOf("fi-alpha-rb-rebuildc", "rebuildc", "rebuild@seed"), metav1.CreateOptions{FieldManager: kubernetes.DefaultFieldManager})
	if err != nil {
		t.Fatal(err)
	}
	spec2, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: meta(names.SpecSecret(set.Name, 2), "rebuildc", "", "")}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Update(func(st *state.State) error {
		st.Fleets["alpha"] = &fleet.Fleet{Name: "alpha", Remote: "git@example.com:alpha.git", Instances: []*fleet.Instance{
			{Name: "rb", Status: fleet.StatusRebuilding, Backend: fleet.BackendKubernetes, KubeCluster: id, KubeKey: "rebuildc"},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kubernetes.BeginJournal(kubernetes.Journal{Op: kubernetes.OpRebuild, Fleet: "alpha", Instance: "rb", Cluster: id, Namespace: ns, Key: "rebuildc", Gen: 2, Step: "seed",
		PrevReplicas: &zero, PrevStatus: fleet.StatusStopped,
		Objects: []kubernetes.JournalObject{
			{Kind: kubernetes.KindStatefulSet, Name: set.Name, UID: string(set.UID), Anchor: true},
			{Kind: kubernetes.KindSecret, Name: spec2.Name, UID: string(spec2.UID), Gen: 2, Created: true},
		}}); err != nil {
		t.Fatal(err)
	}

	svc.failInterruptedJobs()
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	svc.bgCtx = runCtx
	go svc.hub.run(runCtx)
	svc.kubeRecovery.start(runCtx)
	go m.run(runCtx)

	gone := func(get func() error) bool { return get() != nil }
	deadline := time.Now().Add(3 * time.Minute)
	for {
		st, _ := state.Load()
		rb := findInstance(st, "alpha", "rb")
		done := rb != nil && rb.Status == fleet.StatusStopped &&
			gone(func() error {
				_, err := cs.AppsV1().StatefulSets(ns).Get(ctx, "fi-alpha-dead-deadaaaa", metav1.GetOptions{})
				return err
			}) &&
			gone(func() error {
				_, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "fi-alpha-dead-deadaaaa", metav1.GetOptions{})
				return err
			}) &&
			gone(func() error { _, err := cs.CoreV1().Secrets(ns).Get(ctx, sec.Name, metav1.GetOptions{}); return err }) &&
			gone(func() error { _, err := cs.CoreV1().Secrets(ns).Get(ctx, spec2.Name, metav1.GetOptions{}); return err })
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not reconciled: rb %+v, status %v", rb, svc.kube.status(id).GetState())
		}
		time.Sleep(time.Second)
	}
	if got, err := cs.AppsV1().StatefulSets(ns).Get(ctx, set.Name, metav1.GetOptions{}); err != nil || got.Annotations[names.AnnotationOp] != "" {
		t.Fatalf("the undone rebuild's StatefulSet: %v %v", err, got.Annotations)
	}
	for _, key := range []string{"lostaaaa", "rebuildc"} {
		if journalExists(key) {
			t.Errorf("journal %s left", key)
		}
	}
}
