//go:build !nok8s

package preflight

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// fakeDelivery is a Delivery with scripted answers.
type fakeDelivery struct {
	mu sync.Mutex
	// check is Check's answer; checked records what it was asked with.
	check   DeliveryCheck
	local   localcluster.Cluster
	plat    string
	checked bool
	// probeErr fails PullProbe; probed counts its calls.
	probeErr error
	probed   int
	// classified records ClassifyPull's arguments.
	classified []string
}

const probeRef = "gw.example.com/" + "rid/devcontainer@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func (f *fakeDelivery) Check(_ context.Context, local localcluster.Cluster, platform string) DeliveryCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local, f.plat, f.checked = local, platform, true
	return f.check
}

func (f *fakeDelivery) PullProbe(context.Context, string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed++
	if f.probeErr != nil {
		return "", "", f.probeErr
	}
	return probeRef, "fleet-gwpull-0123456789ab-minikube-abc234", nil
}

func (f *fakeDelivery) ClassifyPull(ref, reason, message string) (string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.classified = append(f.classified, ref, reason, message)
	return "tls", "the node does not trust the gateway's certificate", "install the CA on the nodes"
}

func gatewayDelivery() DeliveryCheck {
	return DeliveryCheck{
		Delivery: &fleetgrpc.KubeImageDelivery{Mode: "gateway", GatewayRegistry: true, GatewayManifestOk: true},
		Result:   fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS,
		Detail:   "gateway: route + credential ok (gw.example.com)",
	}
}

// Every full run judges image delivery for the nodes it found, and a
// connection no built image can reach is LIMITED.
func TestFullImageDelivery(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
	d := &fakeDelivery{check: gatewayDelivery()}
	tgt := target(c, fleet.KubeCluster{})
	tgt.Delivery = d
	res := Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if !d.checked || d.local.Kind != localcluster.Minikube || d.plat != "linux/amd64" {
		t.Fatalf("Check asked with %v %q", d.local, d.plat)
	}
	if res.Local.Kind != localcluster.Minikube || res.Platform != "linux/amd64" {
		t.Fatalf("result local %v platform %q", res.Local, res.Platform)
	}
	if got := check(t, res, CheckImageDelivery); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || got.GetDetail() != "gateway: route + credential ok (gw.example.com)" {
		t.Fatalf("image delivery check %v", got)
	}
	if res.Capabilities.GetImageDelivery().GetMode() != "gateway" {
		t.Fatalf("capability %v", res.Capabilities.GetImageDelivery())
	}
	for _, ch := range res.Checks {
		if ch.GetName() == CheckImagePull {
			t.Fatal("a run without Deep ran the deep test")
		}
	}
	if !res.DeepCheckedAt.IsZero() {
		t.Fatal("deep_checked_at set without a deep test")
	}

	d.check = DeliveryCheck{
		Delivery: &fleetgrpc.KubeImageDelivery{Mode: "none", Reason: "no registry is set"},
		Result:   fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "none: no registry is set", Fix: "set a registry",
		Limited: "built images cannot be delivered",
	}
	res = Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "built images cannot be delivered")
	if got := check(t, res, CheckImageDelivery); got.GetFix() != "set a registry" {
		t.Fatalf("image delivery check %v", got)
	}

	// Nodes of two architectures name no one platform.
	arm := cloudNode("arm-1")
	amd := cloudNode("amd-1")
	amd.Status.NodeInfo.Architecture = "amd64"
	if got := nodePlatform([]corev1.Node{*arm, *amd}); got != "" {
		t.Fatalf("mixed platform %q", got)
	}
	if got := nodePlatform([]corev1.Node{*arm}); got != "linux/arm64" {
		t.Fatalf("one arm64 node: %q", got)
	}
}

// probePods records the deep test's pods and answers their status.
type probePods struct {
	mu      sync.Mutex
	created []*corev1.Pod
	deleted []string
	status  func(gets int) corev1.PodStatus
	gets    int
	refuse  error
}

func (p *probePods) install(c *cluster) {
	c.cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !strings.HasPrefix(pod.Name, "fleet-probe-pull-") {
			return false, nil, nil
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.refuse != nil {
			return true, nil, p.refuse
		}
		p.created = append(p.created, pod.DeepCopy())
		return true, pod, nil
	})
	c.cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, pod := range p.created {
			if pod.Name == name {
				p.gets++
				out := pod.DeepCopy()
				out.Status = p.status(p.gets)
				return true, out, nil
			}
		}
		return false, nil, nil
	})
	c.cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.deleted = append(p.deleted, a.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})
}

func waiting(reason, message string) corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "pull", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}}}
}

// The deep test has a pod pull the probe image through the gateway: it
// passes once the image is on the node, names the step a failed pull broke,
// and the pod is always deleted.
func TestDeepImagePull(t *testing.T) {
	defer func(d time.Duration) { pullPoll = d }(pullPoll)
	pullPoll = time.Millisecond
	setup := func(status func(int) corev1.PodStatus) (*cluster, *probePods, *fakeDelivery, Target) {
		c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
		p := &probePods{status: status}
		p.install(c)
		d := &fakeDelivery{check: gatewayDelivery()}
		tgt := target(c, fleet.KubeCluster{
			NodeSelector: map[string]string{"pool": "dev"},
			Tolerations:  []fleet.KubeToleration{{Key: "dedicated", Operator: "Equal", Value: "fleet", Effect: "NoSchedule"}},
		})
		tgt.Delivery, tgt.Deep = d, true
		return c, p, d, tgt
	}

	// Waiting for the pull, then created: the image is on the node.
	_, p, d, tgt := setup(func(gets int) corev1.PodStatus {
		if gets < 3 {
			return waiting("ContainerCreating", "")
		}
		return waiting("RunContainerError", "exec /fleet-probe: permission denied")
	})
	res := Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if got := check(t, res, CheckImagePull); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || !strings.Contains(got.GetDetail(), "pulled the probe image") {
		t.Fatalf("image pull %v", got)
	}
	if res.DeepCheckedAt.IsZero() || d.probed != 1 {
		t.Fatalf("deep checked at %v, probes %d", res.DeepCheckedAt, d.probed)
	}
	if len(p.created) != 1 || len(p.deleted) != 1 || p.deleted[0] != p.created[0].Name {
		t.Fatalf("pods created %d, deleted %v", len(p.created), p.deleted)
	}
	pod := p.created[0]
	spec := pod.Spec
	ctr := spec.Containers[0]
	if ctr.Image != probeRef || ctr.ImagePullPolicy != corev1.PullAlways || len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "fleet-gwpull-0123456789ab-minikube-abc234" {
		t.Fatalf("pull pod container %+v secrets %v", ctr, spec.ImagePullSecrets)
	}
	if pod.Labels[names.LabelRole] != string(names.RoleProbe) || pod.Labels[names.LabelCluster] != "minikube-abc234" || pod.Labels[names.LabelOwner] != "0123456789ab" {
		t.Fatalf("pull pod labels %v", pod.Labels)
	}
	if spec.NodeSelector["pool"] != "dev" || len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "dedicated" {
		t.Fatalf("pull pod placement %v %v", spec.NodeSelector, spec.Tolerations)
	}
	// Hardened, and admissible in a restricted namespace.
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken || pod.Annotations["sidecar.istio.io/inject"] != "false" {
		t.Fatalf("pull pod not hardened: %+v %v", spec, pod.Annotations)
	}
	if msg := restricted(pod); msg != "" {
		t.Fatalf("the pull pod is refused by the restricted level: %s", msg)
	}
	if sc := ctr.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("pull pod security context %+v / %+v", ctr.SecurityContext, spec.SecurityContext)
	}

	// The pull fails: the step is named and the connection is LIMITED.
	_, p, d, tgt = setup(func(int) corev1.PodStatus {
		return waiting("ErrImagePull", "failed to pull: tls: failed to verify certificate: x509: certificate signed by unknown authority")
	})
	res = Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "gateway pull failing (tls)")
	got := check(t, res, CheckImagePull)
	if got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.HasPrefix(got.GetDetail(), "tls: ") || got.GetFix() != "install the CA on the nodes" {
		t.Fatalf("a failed pull %v", got)
	}
	if len(d.classified) != 3 || d.classified[0] != probeRef || d.classified[1] != "ErrImagePull" || len(p.deleted) != 1 {
		t.Fatalf("classified %v, deleted %v", d.classified, p.deleted)
	}

	// The pod cannot be created (a LimitRange minimum, say): a warning,
	// not a verdict on delivery.
	_, p, _, tgt = setup(func(int) corev1.PodStatus { return corev1.PodStatus{} })
	p.refuse = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("minimum cpu usage per Container is 100m"))
	res = Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if got := check(t, res, CheckImagePull); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN || !strings.Contains(got.GetDetail(), "minimum cpu") {
		t.Fatalf("a refused pull pod %v", got)
	}

	// The probe image cannot be served.
	_, p, d, tgt = setup(func(int) corev1.PodStatus { return corev1.PodStatus{} })
	d.probeErr = errors.New("gateway delivery is unavailable: Enable Image Registry is off")
	res = Full(context.Background(), tgt, nil)
	if got := check(t, res, CheckImagePull); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || len(p.created) != 0 {
		t.Fatalf("an unservable probe %v, pods %d", got, len(p.created))
	}

	// Delivery is not the gateway, or the connection is not saved: nothing
	// to pull.
	_, p, d, tgt = setup(func(int) corev1.PodStatus { return corev1.PodStatus{} })
	d.check.Delivery.Mode = "node-load"
	res = Full(context.Background(), tgt, nil)
	if got := check(t, res, CheckImagePull); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED || !strings.Contains(got.GetDetail(), "node-load") || d.probed != 0 || len(p.created) != 0 {
		t.Fatalf("node load %v", got)
	}
	_, _, d, tgt = setup(func(int) corev1.PodStatus { return corev1.PodStatus{} })
	tgt.Cluster.ID = ""
	res = Full(context.Background(), tgt, nil)
	if got := check(t, res, CheckImagePull); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED || d.probed != 0 {
		t.Fatalf("a draft %v", got)
	}
}

// A pod that never pulls (unschedulable, say) ends in a warning naming why
// once the pull timeout passes, and is deleted.
func TestDeepImagePullTimesOut(t *testing.T) {
	defer func(d time.Duration) { pullPoll = d }(pullPoll)
	pullPoll = time.Millisecond
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
	p := &probePods{status: func(int) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity"}}}
	}}
	p.install(c)
	tgt := target(c, fleet.KubeCluster{})
	tgt.Delivery, tgt.Deep = &fakeDelivery{check: gatewayDelivery()}, true
	ctx, cancel := context.WithTimeout(context.Background(), FullTimeout)
	defer cancel()
	// The run's own deadline stands in for the pull timeout.
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{ImageDelivery: gatewayDelivery().Delivery}, nsExists: true}
	pctx, pcancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer pcancel()
	r.imagePull(pctx)
	got := r.res.Checks[len(r.res.Checks)-1]
	if got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN || !strings.Contains(got.GetDetail(), "unschedulable: 0/1 nodes") {
		t.Fatalf("a pod that never pulled %v", got)
	}
	if len(p.deleted) != 1 {
		t.Fatalf("deleted %v", p.deleted)
	}
}
