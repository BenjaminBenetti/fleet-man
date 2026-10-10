//go:build !nok8s

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// With no TUI connected, an instance whose pod keeps running out of memory
// is parked by fleetd on its informers' events alone: scaled to 0, recorded
// stopped, the reason in its warning, the restart on its row. Runs against
// FLEET_KUBE_TEST_CONTEXT in a fleet-it-p2-10b-* namespace (deleted after).
func TestLiveKubePodsParkACrashLoopWithoutATUI(t *testing.T) {
	kubeconfig, kctx := liveKube(t)
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	isolateFleetDir(t)
	link, err := connectKube(fleet.KubeCluster{Context: kctx, KubeconfigPaths: []string{kubeconfig}, Namespace: "default"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	ns := "fleet-it-p2-10b-pool-informers-srv-" + hex.EncodeToString(suffix[:])
	ctx := context.Background()
	if _, err := link.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = link.client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })

	const owner, key = "liveowner123", "liveoomz"
	rec := fleet.KubeCluster{ID: "live-srv-" + hex.EncodeToString(suffix[:]), Context: kctx, KubeconfigPaths: []string{kubeconfig}, Namespace: ns}
	pool := kubernetes.NewPool(kubernetes.PoolOptions{
		Records: func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil },
		Owner:   func() (string, error) { return owner, nil },
	})
	t.Cleanup(pool.Close)

	inst := &fleet.Instance{Name: "oom", Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes, KubeCluster: rec.ID, KubeKey: key, ContainerID: "k8s:" + rec.ID + "/" + ns + "/x@g1"}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}}}}); err != nil {
		t.Fatal(err)
	}
	stsName := names.Instance("alpha", "oom", key)
	labels := names.Base(owner, rec.ID)
	labels[names.LabelRole] = string(names.RoleInstance)
	labels[names.LabelKey] = key
	one, no, grace := int32(1), false, int64(1)
	limit := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("48Mi")}
	if _, err := link.client.AppsV1().StatefulSets(ns).Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: stsName, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one, ServiceName: stsName,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &no, EnableServiceLinks: &no, TerminationGracePeriodSeconds: &grace,
					Containers: []corev1.Container{{
						Name: kubernetes.DevContainer, Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
						Command:   []string{"sh", "-c", "sleep 2; tail /dev/zero"},
						Resources: corev1.ResourceRequirements{Limits: limit, Requests: limit},
					}},
				},
			},
		},
	}, metav1.CreateOptions{FieldManager: kubernetes.DefaultFieldManager}); err != nil {
		t.Fatal(err)
	}

	svc := newService()
	svc.kubePods.pool = func() kubePodPool { return pool }
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go svc.hub.run(runCtx)
	go runStatePoller(runCtx, svc.hub)
	go svc.kubePods.run(runCtx)
	if svc.hub.runtimeWanted.Load() {
		t.Fatal("a TUI is watching")
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		st, err := state.Load()
		if err == nil && st.Fleets["alpha"].Instances[0].Status == fleet.StatusStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the crash-looping instance was not parked: %+v", pool.Observe(rec.ID, stsName))
		}
		time.Sleep(time.Second)
	}
	set, err := link.client.AppsV1().StatefulSets(ns).Get(ctx, stsName, metav1.GetOptions{})
	if err != nil || set.Spec.Replicas == nil || *set.Spec.Replicas != 0 {
		t.Fatalf("the parked StatefulSet: %v %v", set.Spec.Replicas, err)
	}
	warn, err := os.ReadFile(state.WarnPath("alpha", "oom"))
	if err != nil || !strings.HasPrefix(string(warn), kubernetes.CodeOOMKilled+": ") || !strings.Contains(string(warn), "48Mi") {
		t.Fatalf("warning %q %v", warn, err)
	}
	t.Logf("warning: %s", warn)
	got := make(chan string, 1)
	svc.hub.post(func(h *hub) { got <- h.runtime[runtimeKey("alpha", "oom")].GetKube().GetRestartCause() })
	if cause := <-got; cause != string(backend.RestartOOMKilled) {
		t.Fatalf("the row's restart cause %q", cause)
	}
}
