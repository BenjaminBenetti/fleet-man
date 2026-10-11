//go:build !nok8s

package preflight

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

const (
	testNS  = "fleet"
	testUID = "5f0c8a2e-1111-2222-3333-444455556666"
	me      = "minikube-user"
)

var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// cluster is a fake API server shaped like a fresh minikube: one node with
// minikube's labels, the hostpath class, a namespace fleet may do anything
// in.
type cluster struct {
	cs *fake.Clientset
	// deny refuses a dry-run Pod of the shapes it returns a message for.
	deny func(pod *corev1.Pod) string
	// rules are what the rules review grants; incomplete marks it so.
	rules      []authorizationv1.ResourceRule
	incomplete bool
	// allowed answers access reviews.
	allowed func(attr *authorizationv1.ResourceAttributes) bool
}

func newCluster(t *testing.T, objects ...runtime.Object) *cluster {
	t.Helper()
	base := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(testUID)}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}},
	}
	c := &cluster{
		cs:    fake.NewClientset(append(base, objects...)...),
		rules: []authorizationv1.ResourceRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
	}
	c.cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.37.0"}
	c.cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: me, Groups: []string{"system:masters"}}}}, nil
	})
	c.cs.PrependReactor("create", "selfsubjectrulesreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectRulesReview{Status: authorizationv1.SubjectRulesReviewStatus{ResourceRules: c.rules, Incomplete: c.incomplete}}, nil
	})
	c.cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		allowed := c.allowed != nil && c.allowed(review.Spec.ResourceAttributes)
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	c.cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if c.deny != nil {
			if msg := c.deny(pod); msg != "" {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New(msg))
			}
		}
		return true, pod, nil
	})
	return c
}

func minikubeNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "minikube", Labels: map[string]string{"minikube.k8s.io/name": "minikube", corev1.LabelArchStable: "amd64"}},
		Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{Architecture: "amd64"},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
		},
	}
}

func cloudNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: "gce://proj/zone/" + name},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "arm64"}},
	}
}

func storageClass(name, provisioner string, opts ...func(*storagev1.StorageClass)) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner}
	for _, o := range opts {
		o(sc)
	}
	return sc
}

func isDefault(sc *storagev1.StorageClass) {
	sc.Annotations = map[string]string{annotationDefaultClass: "true"}
}

func wffc(sc *storagev1.StorageClass) {
	m := storagev1.VolumeBindingWaitForFirstConsumer
	sc.VolumeBindingMode = &m
}

func expandable(sc *storagev1.StorageClass) {
	yes := true
	sc.AllowVolumeExpansion = &yes
}

func retain(sc *storagev1.StorageClass) {
	p := corev1.PersistentVolumeReclaimRetain
	sc.ReclaimPolicy = &p
}

// baseline refuses what the pod-security baseline level refuses.
func baseline(pod *corev1.Pod) string {
	sc := pod.Spec.Containers[0].SecurityContext
	switch {
	case sc != nil && sc.Privileged != nil && *sc.Privileged:
		return `violates PodSecurity "baseline:latest": privileged (container "dev" must not set securityContext.privileged=true)`
	case sc != nil && sc.Capabilities != nil && len(sc.Capabilities.Add) > 0:
		return `violates PodSecurity "baseline:latest": non-default capabilities (container "dev" must not include "SYS_PTRACE" in securityContext.capabilities.add)`
	case sc != nil && sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined:
		return `violates PodSecurity "baseline:latest": seccompProfile (container must not set securityContext.seccompProfile.type to "Unconfined")`
	}
	return ""
}

// restricted refuses root as the restricted level does (on top of
// baseline).
func restricted(pod *corev1.Pod) string {
	if msg := baseline(pod); msg != "" {
		return msg
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		return `violates PodSecurity "restricted:latest": runAsNonRoot != true`
	}
	return ""
}

type fakeLease struct {
	st  owner.LeaseStatus
	err error
}

func (l fakeLease) Ensure(context.Context) (owner.LeaseStatus, error) { return l.st, l.err }

func target(c *cluster, rec fleet.KubeCluster) Target {
	if rec.ID == "" {
		rec.ID = "minikube-abc234"
	}
	if rec.Namespace == "" {
		rec.Namespace = testNS
	}
	if rec.Context == "" {
		rec.Context = "minikube"
	}
	return Target{
		Cluster: rec, Client: c.cs, Auth: kubeexec.AuthClientCert, Source: "/home/me/.kube/config",
		Credential: func(context.Context) (kubeexec.Credential, error) {
			return kubeexec.Credential{Auth: kubeexec.AuthClientCert, ExpiresAt: now.Add(365 * 24 * time.Hour)}, nil
		},
		Lease: fakeLease{st: owner.LeaseStatus{Held: true}},
		Owner: "0123456789ab",
		Now:   func() time.Time { return now },
	}
}

func check(t *testing.T, res Result, name string) *fleetgrpc.KubeCheck {
	t.Helper()
	for _, c := range res.Checks {
		if c.GetName() == name {
			return c
		}
	}
	t.Fatalf("no %s check in %v", name, res.Checks)
	return nil
}

func wantState(t *testing.T, res Result, state fleetgrpc.KubeClusterState, msgPart string) {
	t.Helper()
	if res.State != state || !strings.Contains(res.Message, msgPart) {
		t.Fatalf("state = %v %q, want %v containing %q\nchecks: %v", res.State, res.Message, state, msgPart, res.Checks)
	}
}

// A fresh minikube is READY: privileged pods are admitted and allowed on a
// local cluster, and that is said as a warning.
func TestFullOnMinikubeIsReady(t *testing.T) {
	c := newCluster(t, minikubeNode(),
		storageClass("standard", "k8s.io/minikube-hostpath", isDefault),
		storageClass("csi-hostpath-sc", "hostpath.csi.k8s.io"))
	tgt := target(c, fleet.KubeCluster{})
	tgt.Raw = func(_ context.Context, path string) ([]byte, error) {
		if path != "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses" {
			return nil, apierrors.NewNotFound(schema.GroupResource{}, path)
		}
		return []byte(`{"items":[{"metadata":{"name":"csi-hostpath-snapclass"},"driver":"hostpath.csi.k8s.io"}]}`), nil
	}
	var reported []string
	res := Full(context.Background(), tgt, func(c *fleetgrpc.KubeCheck) {
		reported = append(reported, c.GetName()+":"+strings.TrimPrefix(c.GetResult().String(), "KUBE_CHECK_RESULT_"))
	})
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	caps := res.Capabilities
	if caps.GetVersion() != "v1.37.0" || caps.GetUser() != me || !caps.GetLocalCluster() || !caps.GetPrivilegedAllowed() {
		t.Errorf("capabilities = %v", caps)
	}
	if a := caps.GetAdmission(); !a.GetUnprivileged() || !a.GetDebugCapabilities() || !a.GetPrivileged() || !caps.GetBuildkitAdmitted() {
		t.Errorf("admission = %v, buildkit %v", a, caps.GetBuildkitAdmitted())
	}
	if res.ClusterUID != testUID || len(caps.GetNodeArchs()) != 1 || caps.GetNodeArchs()[0] != "amd64" {
		t.Errorf("uid %q archs %v", res.ClusterUID, caps.GetNodeArchs())
	}
	if got := check(t, res, CheckRootStorage).GetDetail(); got != "auto → csi-hostpath-sc (RWO, Immediate, node-local)" {
		t.Errorf("root storage = %q (the snapshot-capable class should win)", got)
	}
	if caps.GetCloneTier() != "snapshot" || len(caps.GetSnapshotClasses()) != 1 {
		t.Errorf("clone tier %q snapshots %v", caps.GetCloneTier(), caps.GetSnapshotClasses())
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "privileged allowed") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	// Each check is reported running, then with its result, in check order.
	want := []string{
		"kubeconfig:PASS", "credentials:RUNNING", "credentials:PASS", "reachable:RUNNING", "reachable:PASS",
		"identity:RUNNING", "identity:PASS", "namespace:RUNNING", "namespace:PASS", "rbac:RUNNING", "rbac:PASS",
		"owner:RUNNING", "owner:PASS", "nodes:RUNNING", "nodes:PASS",
	}
	if strings.Join(reported[:len(want)], " ") != strings.Join(want, " ") {
		t.Errorf("reported\n %v\nwant prefix\n %v", reported, want)
	}
	if !strings.HasPrefix(check(t, res, CheckCredentials).GetDetail(), "client certificate (expires in 365d") {
		t.Errorf("credentials detail = %q", check(t, res, CheckCredentials).GetDetail())
	}
}

// A namespace labelled pod-security baseline is LIMITED: privileged and the
// debug capabilities are denied, and nothing else.
func TestFullBaselineNamespaceIsLimited(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "baseline-ns", Labels: map[string]string{labelPSAEnforce: "baseline"}}}
	c := newCluster(t, minikubeNode(), ns, storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
	c.deny = baseline
	res := Full(context.Background(), target(c, fleet.KubeCluster{Namespace: "baseline-ns"}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "debug caps denied · privileged denied")
	a := res.Capabilities.GetAdmission()
	if !a.GetUnprivileged() || a.GetDebugCapabilities() || a.GetPrivileged() || !strings.Contains(a.GetDenial(), "baseline") {
		t.Errorf("admission = %v", a)
	}
	if res.Capabilities.GetPsaLevel() != "baseline" {
		t.Errorf("psa level = %q", res.Capabilities.GetPsaLevel())
	}
	if c := check(t, res, CheckAdmissionPrivileged); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.HasPrefix(c.GetDetail(), `violates PodSecurity "baseline:latest"`) {
		t.Errorf("privileged check = %v", c)
	}
	if c := check(t, res, CheckAdmissionDebug); !strings.Contains(c.GetDetail(), "dropped with a warning") {
		t.Errorf("debug check = %v", c)
	}
	// The rootless buildkit asks for unconfined seccomp, which baseline
	// forbids.
	if res.Capabilities.GetBuildkitAdmitted() || !strings.HasPrefix(res.Capabilities.GetBuildkitReason(), "Shared Buildkit unavailable on this cluster") {
		t.Errorf("buildkit = %v %q", res.Capabilities.GetBuildkitAdmitted(), res.Capabilities.GetBuildkitReason())
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "privileged allowed") {
			t.Errorf("warns privileged allowed where it is denied: %v", res.Warnings)
		}
	}
}

// A restricted namespace still runs instances as one non-root user (tier C);
// one that admits no pod fleet can render is MISCONFIGURED.
func TestFullRootDenied(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))
	c.deny = restricted
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "restricted: no root")
	if res.Capabilities.GetAudioCapable() {
		t.Error("a restricted namespace is not audio capable without a baked stack")
	}

	c.deny = func(*corev1.Pod) string { return `admission webhook "policy.example" denied the request: no pods here` }
	res = Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, "no pods here")
}

// Off a local cluster privileged pods are off until allowed: admitted but not
// allowed is LIMITED; allowed is READY with the node warning.
func TestFullPrivilegedPolicyOffLocal(t *testing.T) {
	c := newCluster(t, cloudNode("gke-a"), cloudNode("gke-b"), storageClass("standard-rwo", "pd.csi.storage.gke.io", isDefault, wffc, expandable))
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "privileged not allowed")
	if res.Capabilities.GetLocalCluster() || res.Capabilities.GetPrivilegedAllowed() || !res.Capabilities.GetAdmission().GetPrivileged() {
		t.Errorf("capabilities = %v", res.Capabilities)
	}
	if got := check(t, res, CheckRootStorage).GetDetail(); got != "auto → standard-rwo (RWO, WFFC, expandable, network)" {
		t.Errorf("root storage = %q", got)
	}

	yes := true
	res = Full(context.Background(), target(c, fleet.KubeCluster{AllowPrivileged: &yes}), nil)
	// Allowed, privileged pods no longer limit it: only its fleets' shares
	// do, as no class of the cluster can pass the multi-node test.
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, LimitedOneNode)
	if strings.Contains(res.Message, "privileged") {
		t.Errorf("still limited by privileged pods: %q", res.Message)
	}
	if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "node is the isolation boundary") || res.Warnings[1] != OneNodeWarning {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestFullNamespaceMissing(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))
	res := Full(context.Background(), target(c, fleet.KubeCluster{Namespace: "nope"}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, "namespace 'nope' does not exist")
	for _, name := range []string{CheckRBAC, CheckAdmissionUnprivileged, CheckLimits, CheckQuota, CheckExposure} {
		if c := check(t, res, name); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED {
			t.Errorf("%s = %v, want skipped", name, c)
		}
	}
	if f := check(t, res, CheckNamespace).GetFix(); !strings.Contains(f, "create it") {
		t.Errorf("namespace fix = %q", f)
	}
}

// Missing required verbs are FORBIDDEN and named; where the rules review is
// incomplete (a webhook authorizer) access reviews decide.
func TestFullRBAC(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))
	c.rules = []authorizationv1.ResourceRule{
		{Verbs: []string{"*"}, APIGroups: []string{"", "apps", "batch", "networking.k8s.io", "coordination.k8s.io"}, Resources: []string{"*"}},
	}
	c.rules = append(c.rules, authorizationv1.ResourceRule{Verbs: []string{"create"}, APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"one"}})
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if ch := check(t, res, CheckRBAC); ch.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN || !strings.Contains(ch.GetDetail(), "optional missing: create volumesnapshots") {
		t.Errorf("rbac = %v", ch)
	}

	c.rules = []authorizationv1.ResourceRule{{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{"*"}, Resources: []string{"*"}}}
	res = Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN, "missing create statefulsets")
	if !strings.Contains(check(t, res, CheckRBAC).GetFix(), me) {
		t.Errorf("rbac fix should name the identity: %v", check(t, res, CheckRBAC))
	}

	asked := 0
	c.incomplete = true
	c.allowed = func(*authorizationv1.ResourceAttributes) bool { asked++; return true }
	res = Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if asked == 0 {
		t.Error("an incomplete rules review must fall back to access reviews")
	}
}

// TestFullRBACDisruptionBudget: podspec applies the budget server-side and
// prune lists it, so protect_from_drain needs patch and list on it too.
func TestFullRBACDisruptionBudget(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))
	all := authorizationv1.ResourceRule{Verbs: []string{"*"}, APIGroups: []string{"", "apps", "batch", "networking.k8s.io", "coordination.k8s.io"}, Resources: []string{"*"}}
	for _, verb := range []string{"patch", "list"} {
		var verbs []string
		for _, v := range []string{"create", "get", "list", "patch", "delete"} {
			if v != verb {
				verbs = append(verbs, v)
			}
		}
		c.rules = []authorizationv1.ResourceRule{all, {Verbs: verbs, APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}}}
		res := Full(context.Background(), target(c, fleet.KubeCluster{ProtectFromDrain: true}), nil)
		wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN, "missing "+verb+" poddisruptionbudgets")
	}
	c.rules = []authorizationv1.ResourceRule{all, {Verbs: []string{"create", "get", "list", "patch", "delete"}, APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}}}
	res := Full(context.Background(), target(c, fleet.KubeCluster{ProtectFromDrain: true}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
}

func TestFullOwnerConflict(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))
	tgt := target(c, fleet.KubeCluster{})
	tgt.Lease = fakeLease{st: owner.LeaseStatus{Holder: "0123456789ab-bbbb", HolderHost: "desktop", RenewedAt: now.Add(-10 * time.Second), FreeAt: now.Add(80 * time.Second)}}
	res := Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT, "on desktop, renewed 10s ago")
	if res.Lease == nil || res.Lease.Held {
		t.Errorf("lease = %+v", res.Lease)
	}

	// An unsaved connection claims nothing.
	tgt.Lease = nil
	res = Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	for _, ch := range res.Checks {
		if ch.GetName() == CheckOwner {
			t.Errorf("an unsaved connection ran the owner check: %v", ch)
		}
	}
}

func TestFullLimitsAndQuota(t *testing.T) {
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "lr", Namespace: testNS}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
		Type:    corev1.LimitTypeContainer,
		Default: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}}}}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: testNS}, Status: corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("10Gi"), corev1.ResourcePods: resource.MustParse("10")},
		Used: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("3Gi"), corev1.ResourcePods: resource.MustParse("2")},
	}}
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"), lr, quota)
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if ch := check(t, res, CheckLimits); ch.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN || ch.GetDetail() != "LimitRange default memory 1Gi → explicit 8Gi limit" {
		t.Errorf("limits = %v", ch)
	}
	if res.Capabilities.GetLimitRangeMemoryLimit() != "1Gi" {
		t.Errorf("limit range memory = %q", res.Capabilities.GetLimitRangeMemoryLimit())
	}
	if ch := check(t, res, CheckQuota); ch.GetDetail() != "room for 3 more instances" {
		t.Errorf("quota = %v", ch)
	}
	if len(res.Capabilities.GetQuota()) != 2 {
		t.Errorf("quota headroom = %v", res.Capabilities.GetQuota())
	}

	// A maximum below fleet's limit and a full quota degrade the connection.
	lr.Spec.Limits[0].Max = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}
	quota.Status.Used[corev1.ResourceRequestsMemory] = resource.MustParse("9Gi")
	c = newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"), lr, quota)
	res = Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "memory limit above the LimitRange maximum · quota full")
	if f := check(t, res, CheckLimits).GetFix(); f != "set the memory limit to at most 4Gi" {
		t.Errorf("limits fix = %q", f)
	}
}

// A ResourceQuota that fleet's own instances filled refuses even the dry-run
// canaries ("exceeded quota"), but quota is judged after pod security and
// the policy webhooks: the namespace still admits fleet's pods, and the
// connection is LIMITED by the full quota, never MISCONFIGURED.
func TestFullQuotaExhaustedIsNotAnAdmissionDenial(t *testing.T) {
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: testNS}, Status: corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("4")},
		Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("4")},
	}}
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"), quota)
	c.deny = func(*corev1.Pod) string {
		return "exceeded quota: q, requested: pods=1, used: pods=4, limited: pods=4"
	}
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "quota full")
	for _, name := range []string{CheckAdmissionUnprivileged, CheckAdmissionDebug, CheckAdmissionPrivileged, CheckBuildkit} {
		if ch := check(t, res, name); ch.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
			t.Errorf("%s = %v", name, ch)
		}
	}
	if a := res.Capabilities.GetAdmission(); !a.GetUnprivileged() || !a.GetPrivileged() {
		t.Errorf("admission = %v", a)
	}
}

// The metrics probe goes through the context-aware raw reader, bounded like
// every other call: discovery's own call takes no context, and a stalled API
// server would hold the run (and the Lease renewals behind it) for good.
func TestFullMetricsProbeIsBounded(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))
	tgt := target(c, fleet.KubeCluster{})
	bounded := false
	tgt.Raw = func(ctx context.Context, path string) ([]byte, error) {
		if path == "/apis/metrics.k8s.io/v1beta1" {
			_, bounded = ctx.Deadline()
			return []byte(`{"kind":"APIResourceList","groupVersion":"metrics.k8s.io/v1beta1"}`), nil
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{}, path)
	}
	res := Full(context.Background(), tgt, nil)
	if !bounded {
		t.Error("the metrics probe ran without a deadline")
	}
	if !res.Capabilities.GetMetricsAvailable() || check(t, res, CheckMetrics).GetDetail() != "metrics.k8s.io available" {
		t.Errorf("metrics = %v (%v)", res.Capabilities.GetMetricsAvailable(), check(t, res, CheckMetrics))
	}
}

func TestFullStorage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   []runtime.Object
		classes []runtime.Object
		rec     fleet.KubeCluster
		state   fleetgrpc.KubeClusterState
		msg     string
		detail  string
		warning string
	}{
		{name: "no class", nodes: []runtime.Object{minikubeNode()},
			state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg: "no StorageClass"},
		{name: "named class missing", nodes: []runtime.Object{minikubeNode()}, classes: []runtime.Object{storageClass("standard", "k8s.io/minikube-hostpath")},
			rec: fleet.KubeCluster{StorageClass: "fast"}, state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg: `storage class "fast" does not exist`},
		{name: "node-local on several nodes", nodes: []runtime.Object{cloudNode("a"), cloudNode("b")}, classes: []runtime.Object{storageClass("local-path", "rancher.io/local-path", isDefault)},
			state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, msg: "node-local storage", warning: "data lost if node replaced"},
		{name: "node-local acknowledged", nodes: []runtime.Object{cloudNode("a"), cloudNode("b")}, classes: []runtime.Object{storageClass("local-path", "rancher.io/local-path", isDefault)},
			rec: fleet.KubeCluster{NodeLocalAck: true}, state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, msg: "privileged not allowed", warning: "data lost if node replaced"},
		{name: "network beats node-local on several nodes", nodes: []runtime.Object{cloudNode("a"), cloudNode("b")},
			classes: []runtime.Object{storageClass("local-path", "rancher.io/local-path", isDefault, wffc), storageClass("gp3", "ebs.csi.aws.com", wffc, retain)},
			state:   fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, msg: "privileged not allowed", detail: "auto → gp3 (RWO, WFFC, network): reclaim policy Retain: disks outlive the instances they belonged to", warning: "keeps disks after destroy"},
		// A class of hand-made volumes (local static PVs) outscores minikube's
		// Immediate default on WaitForFirstConsumer, but a new disk there
		// would wait for a volume nobody makes: auto never picks it.
		{name: "static volumes are never auto's", nodes: []runtime.Object{minikubeNode()},
			classes: []runtime.Object{storageClass("standard", "k8s.io/minikube-hostpath", isDefault), storageClass("local-storage", "kubernetes.io/no-provisioner", wffc)},
			state:   fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, detail: "auto → standard (RWO, Immediate, node-local)"},
		{name: "only static volumes", nodes: []runtime.Object{minikubeNode()}, classes: []runtime.Object{storageClass("local-storage", "kubernetes.io/no-provisioner", wffc)},
			state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg: "no StorageClass that provisions volumes"},
		{name: "static volumes when named", nodes: []runtime.Object{minikubeNode()}, classes: []runtime.Object{storageClass("local-storage", "kubernetes.io/no-provisioner", wffc)},
			rec: fleet.KubeCluster{StorageClass: "local-storage"}, state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, detail: "local-storage (RWO, WFFC, node-local)"},
		{name: "nfs only", nodes: []runtime.Object{minikubeNode()}, classes: []runtime.Object{storageClass("nfs", "nfs.csi.k8s.io")},
			state: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, msg: "instance disks on a file share", detail: "auto → nfs (file share, Immediate, network): an NFS class for instance disks: root_squash breaks package installs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, append(tc.nodes, tc.classes...)...)
			res := Full(context.Background(), target(c, tc.rec), nil)
			wantState(t, res, tc.state, tc.msg)
			if tc.detail != "" {
				if got := check(t, res, CheckRootStorage).GetDetail(); got != tc.detail {
					t.Errorf("root storage = %q, want %q", got, tc.detail)
				}
			}
			if tc.warning != "" && !strings.Contains(strings.Join(res.Warnings, "\n"), tc.warning) {
				t.Errorf("warnings = %v, want %q", res.Warnings, tc.warning)
			}
		})
	}
}

// Other subjects that can exec into, create or patch pods, or mount the
// share, are named; fleet's own identity is not.
func TestFullExposure(t *testing.T) {
	edit := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "edit"}, Rules: []rbacv1.PolicyRule{
		{Verbs: []string{"create"}, APIGroups: []string{""}, Resources: []string{"pods", "pods/exec", "persistentvolumeclaims"}},
	}}
	viewer := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "viewer", Namespace: testNS}, Rules: []rbacv1.PolicyRule{
		{Verbs: []string{"get", "list"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
	}}
	bindings := []runtime.Object{
		edit, viewer,
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "team-edit", Namespace: testNS}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "edit"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "alice"}, {Kind: rbacv1.UserKind, Name: me}, {Kind: rbacv1.ServiceAccountKind, Name: "ci"}}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "view", Namespace: testNS}, RoleRef: rbacv1.RoleRef{Kind: "Role", Name: "viewer"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "auditors"}}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: testNS}, RoleRef: rbacv1.RoleRef{Kind: "Role", Name: "gone"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "bob"}}},
	}
	c := newCluster(t, append([]runtime.Object{minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath")}, bindings...)...)
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	// Exposure never degrades the state.
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	ch := check(t, res, CheckExposure)
	want := "2 other subjects can act inside your instances: ServiceAccount fleet/ci (pods/exec, pod create), User alice (pods/exec, pod create); " +
		"2 other subjects can read your agent logins: ServiceAccount fleet/ci (pod create, PVC create), User alice (pod create, PVC create) (1 binding to roles fleet cannot read)"
	if ch.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN || ch.GetDetail() != want {
		t.Errorf("exposure =\n %q\nwant\n %q", ch.GetDetail(), want)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "can act inside your instances") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestFullCredentialAndReachFailures(t *testing.T) {
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath"))

	tgt := target(c, fleet.KubeCluster{})
	tgt.Auth, tgt.Plugin = kubeexec.AuthExecPlugin, "aws"
	tgt.Credential = func(context.Context) (kubeexec.Credential, error) {
		return kubeexec.Credential{}, &kubeexec.PluginError{Path: "/usr/bin/aws", ExitCode: 255, Stderr: "Error loading SSO Token: token expired"}
	}
	res := Full(context.Background(), tgt, nil)
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, "Error loading SSO Token")
	if f := check(t, res, CheckCredentials).GetFix(); !strings.Contains(f, "Run login on daemon") {
		t.Errorf("credentials fix = %q", f)
	}
	if len(res.Checks) != 2 {
		t.Errorf("checks after a failed credential: %v", res.Checks)
	}

	tgt = target(c, fleet.KubeCluster{})
	tgt.Credential = func(context.Context) (kubeexec.Credential, error) {
		return kubeexec.Credential{Auth: kubeexec.AuthClientCert, ExpiresAt: now.Add(-2 * time.Hour)}, nil
	}
	wantState(t, Full(context.Background(), tgt, nil), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, "credentials expired 2h ago")

	for _, tc := range []struct {
		err   error
		state fleetgrpc.KubeClusterState
	}{
		{&url.Error{Op: "Get", URL: "https://10.0.0.1:6443/version", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE},
		{apierrors.NewUnauthorized("Unauthorized"), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED},
		{&url.Error{Op: "Get", URL: "https://x/version", Err: errors.New("getting credentials: exec: executable aws failed with exit code 255")}, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED},
	} {
		c := newCluster(t)
		c.cs.Discovery().(*fakediscovery.FakeDiscovery).PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, tc.err
		})
		res := Shallow(context.Background(), target(c, fleet.KubeCluster{}))
		if res.State != tc.state {
			t.Errorf("Shallow with %v = %v %q, want %v", tc.err, res.State, res.Message, tc.state)
		}
	}
}

func TestShallow(t *testing.T) {
	c := newCluster(t)
	res := Shallow(context.Background(), target(c, fleet.KubeCluster{}))
	wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, "")
	if res.Capabilities != nil || res.Lease == nil || !res.Lease.Held {
		t.Errorf("shallow result = %+v", res)
	}
	tgt := target(c, fleet.KubeCluster{})
	tgt.Lease = fakeLease{st: owner.LeaseStatus{HolderHost: "desktop"}}
	wantState(t, Shallow(context.Background(), tgt), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT, "desktop")
}

func TestClassifyAndLoadFailure(t *testing.T) {
	for _, tc := range []struct {
		err   error
		state fleetgrpc.KubeClusterState
	}{
		{fmt.Errorf("%w: gone", kubeexec.ErrKubeconfigMissing), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING},
		{fmt.Errorf("%w: gone", kubeexec.ErrContextMissing), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING},
		{fmt.Errorf("%w: aws", kubeexec.ErrPluginMissing), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING},
		{fmt.Errorf("%w: oidc", kubeexec.ErrMisconfigured), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("nope")), fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN},
		{context.DeadlineExceeded, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE},
	} {
		if got, _ := Classify(tc.err); got != tc.state {
			t.Errorf("Classify(%v) = %v, want %v", tc.err, got, tc.state)
		}
	}
	res := LoadFailure(fmt.Errorf("%w: plugin \x1b[31maws\x1b[0m not found", kubeexec.ErrPluginMissing), now)
	if res.State != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING || strings.Contains(res.Message, "\x1b") || len(res.Checks) != 1 || res.Checks[0].GetFix() == "" {
		t.Errorf("LoadFailure = %+v", res)
	}
}

func TestRulesAllow(t *testing.T) {
	rules := []authorizationv1.ResourceRule{
		{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"*/log"}},
		{Verbs: []string{"create"}, APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}},
		{Verbs: []string{"*"}, APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"one"}},
	}
	for _, tc := range []struct {
		group, resource, verb string
		want                  bool
	}{
		{"", "pods/log", "get", true},
		{"", "pods", "get", false},
		{"apps", "statefulsets", "create", true},
		{"", "statefulsets", "create", false},
		{"", "secrets", "create", false},
	} {
		if got := rulesAllow(rules, tc.group, tc.resource, tc.verb); got != tc.want {
			t.Errorf("rulesAllow(%s %s/%s) = %v", tc.verb, tc.group, tc.resource, got)
		}
	}
}

// A class is a CSI clone source only for a CSI driver fleet knows clones:
// minikube's hostpath provisioner takes a data source it does not know for an
// empty disk.
func TestClassifyStorageClassCSIClone(t *testing.T) {
	drivers := map[string]*storagev1.CSIDriver{"hostpath.csi.k8s.io": {}, "ebs.csi.aws.com": {}}
	for _, tc := range []struct {
		provisioner string
		want        bool
	}{
		{"hostpath.csi.k8s.io", true},
		{"ebs.csi.aws.com", false},
		{"k8s.io/minikube-hostpath", false},
		{"pd.csi.storage.gke.io", false}, // known, but no CSIDriver object here
	} {
		got := ClassifyStorageClass(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Provisioner: tc.provisioner}, drivers).GetCsiClone()
		if got != tc.want {
			t.Errorf("%s: csi_clone %v, want %v", tc.provisioner, got, tc.want)
		}
	}
}
