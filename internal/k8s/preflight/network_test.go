//go:build !nok8s

package preflight

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

func TestParseNetworkProbe(t *testing.T) {
	for _, tt := range []struct {
		out            string
		metadata, peer bool
		bad            bool
	}{
		{out: "metadata=unreached\npeer=unreached\n"},
		{out: "metadata=reached\npeer=reached\n", metadata: true, peer: true},
		{out: "wget: noise\nmetadata=unreached\npeer=reached\n", peer: true},
		{out: "metadata=unreached\n", bad: true},
		{out: "fake logs", bad: true},
	} {
		v, err := parseNetworkProbe(tt.out)
		if (err != nil) != tt.bad {
			t.Errorf("%q: err %v", tt.out, err)
			continue
		}
		if !tt.bad && (v.metadataReachable != tt.metadata || v.peerReachable != tt.peer) {
			t.Errorf("%q: %+v", tt.out, v)
		}
	}
}

// The client tries the metadata address and the target, one wget each, and
// counts any HTTP answer (busybox's "server returned error") as reached.
func TestProbeScript(t *testing.T) {
	s := probeScript("10.0.0.7")
	for _, want := range []string{"probe metadata http://169.254.169.254/", "probe peer http://10.0.0.7:8080/", `*"server returned error"*) rc=0`} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if s := probeScript("fd00::7"); !strings.Contains(s, "http://[fd00::7]:8080/") {
		t.Errorf("an IPv6 target is not bracketed:\n%s", s)
	}
}

// fakeNetwork answers the probe's pods as a cluster would: the target runs
// with an IP, the client finishes, and its log is what the test says.
type fakeNetwork struct {
	mu       sync.Mutex
	pods     map[string]*corev1.Pod
	policies []*networkingv1.NetworkPolicy
	deleted  []string
}

func (f *fakeNetwork) install(c *cluster) {
	f.pods = map[string]*corev1.Pod{}
	c.cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !strings.HasPrefix(pod.Name, "fleet-probe-net-") {
			return false, nil, nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pods[pod.Name] = pod.DeepCopy()
		return true, pod, nil
	})
	c.cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		f.mu.Lock()
		defer f.mu.Unlock()
		pod, ok := f.pods[name]
		if !ok {
			return false, nil, nil
		}
		out := pod.DeepCopy()
		if strings.HasSuffix(name, "-target") {
			out.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.7"}
		} else {
			out.Status = corev1.PodStatus{Phase: corev1.PodSucceeded}
		}
		return true, out, nil
	})
	c.cs.PrependReactor("create", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		np := a.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.policies = append(f.policies, np.DeepCopy())
		return true, np, nil
	})
	for _, res := range []string{"pods", "networkpolicies"} {
		res := res
		c.cs.PrependReactor("delete", res, func(a k8stesting.Action) (bool, runtime.Object, error) {
			name := a.(k8stesting.DeleteAction).GetName()
			if !strings.HasPrefix(name, "fleet-probe-net-") {
				return false, nil, nil
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deleted = append(f.deleted, res+"/"+name)
			return true, nil, nil
		})
	}
}

func stubClientLog(t *testing.T, out string) {
	t.Helper()
	orig := clientLog
	clientLog = func(*run, context.Context, string) (string, error) { return out, nil }
	t.Cleanup(func() { clientLog = orig })
}

// The deep test's network probe: a target closed by a NetworkPolicy and a
// client under a fleet pod's egress rules; what the client reached sets the
// capabilities and the checks, and everything it made is deleted.
func TestDeepNetworkProbe(t *testing.T) {
	defer func(d time.Duration) { networkPoll = d }(networkPoll)
	networkPoll = time.Millisecond
	off := false
	for _, tt := range []struct {
		name               string
		log                string
		block              *bool
		enforced, metadata bool
		policyResult       fleetgrpc.KubeCheckResult
		metadataResult     fleetgrpc.KubeCheckResult
		metadataDetail     string
		warning            string
	}{
		{name: "enforced and blocked", log: "metadata=unreached\npeer=unreached\n", enforced: true,
			policyResult: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, metadataResult: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS,
			metadataDetail: "not reachable from pods (egress block on)"},
		{name: "not enforced", log: "metadata=reached\npeer=reached\n", metadata: true,
			policyResult: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, metadataResult: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN,
			metadataDetail: "169.254.169.254 reachable from pods (egress block on)", warning: "NetworkPolicies are not enforced here"},
		{name: "block off", log: "metadata=reached\npeer=unreached\n", block: &off, enforced: true, metadata: true,
			policyResult: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, metadataResult: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN,
			metadataDetail: "reachable from pods (egress block off)", warning: "169.254.169.254 reachable from pods (egress block off)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
			f := &fakeNetwork{}
			f.install(c)
			stubClientLog(t, tt.log)
			tgt := target(c, fleet.KubeCluster{BlockMetadata: tt.block, NodeSelector: map[string]string{"pool": "dev"}})
			r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true}
			r.networkProbe(context.Background())

			if r.caps.GetNetworkPolicyEnforced() != tt.enforced || r.caps.GetMetadataReachable() != tt.metadata {
				t.Fatalf("caps enforced=%v metadata=%v", r.caps.GetNetworkPolicyEnforced(), r.caps.GetMetadataReachable())
			}
			if got := check(t, r.res, CheckNetworkPolicy); got.GetResult() != tt.policyResult {
				t.Errorf("network_policy %v", got)
			}
			if got := check(t, r.res, CheckMetadata); got.GetResult() != tt.metadataResult || !strings.Contains(got.GetDetail(), tt.metadataDetail) {
				t.Errorf("metadata %v", got)
			}
			found := tt.warning == ""
			for _, w := range r.res.Warnings {
				if strings.Contains(w, tt.warning) && tt.warning != "" {
					found = true
				}
			}
			if !found {
				t.Errorf("warnings %q lack %q", r.res.Warnings, tt.warning)
			}

			// The probe's objects: the closed target's policy, the client's
			// egress rules while the connection blocks metadata, two pods.
			wantPolicies := 2
			if tt.block != nil && !*tt.block {
				wantPolicies = 1
			}
			if len(f.policies) != wantPolicies {
				t.Fatalf("policies %d, want %d", len(f.policies), wantPolicies)
			}
			closed := f.policies[0]
			if len(closed.Spec.Ingress) != 0 || closed.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || !strings.HasSuffix(closed.Spec.PodSelector.MatchLabels[labelProbe], "-target") {
				t.Errorf("target policy %+v", closed.Spec)
			}
			if wantPolicies == 2 {
				eg := f.policies[1]
				if !strings.HasSuffix(eg.Spec.PodSelector.MatchLabels[labelProbe], "-client") || len(eg.Spec.Egress) == 0 ||
					eg.Spec.Egress[0].To[0].IPBlock.Except[0] != "169.254.0.0/16" {
					t.Errorf("client egress policy %+v", eg.Spec)
				}
			}
			if len(f.pods) != 2 {
				t.Fatalf("pods %d", len(f.pods))
			}
			for name, pod := range f.pods {
				if pod.Spec.Containers[0].Image != probeImage || pod.Labels[names.LabelRole] != string(names.RoleProbe) ||
					pod.Labels[names.LabelCluster] != "minikube-abc234" || pod.Spec.NodeSelector["pool"] != "dev" {
					t.Errorf("pod %s: %+v %v", name, pod.Spec.Containers[0], pod.Labels)
				}
				if v := podspec.CheckHardened(pod.ObjectMeta, pod.Spec, podspec.HardenOptions{PinnedImages: true}); len(v) > 0 {
					t.Errorf("pod %s not hardened: %v", name, v)
				}
				if sc := pod.Spec.SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
					t.Errorf("pod %s runs as root", name)
				}
			}
			if len(f.deleted) != len(f.pods)+len(f.policies) {
				t.Errorf("deleted %v, want every pod and policy", f.deleted)
			}
		})
	}
}

// An unsaved connection, or a missing namespace, has nothing to probe.
func TestDeepNetworkProbeSkips(t *testing.T) {
	c := newCluster(t)
	tgt := target(c, fleet.KubeCluster{})
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}}
	r.networkProbe(context.Background())
	if got := check(t, r.res, CheckMetadata); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED {
		t.Fatalf("metadata %v", got)
	}
	tgt.Cluster.ID = ""
	r = &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true}
	r.networkProbe(context.Background())
	if got := check(t, r.res, CheckNetworkPolicy); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED {
		t.Fatalf("network_policy %v", got)
	}
}
