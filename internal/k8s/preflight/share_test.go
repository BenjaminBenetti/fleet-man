//go:build !nok8s

package preflight

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func immediateOne(sc *storagev1.StorageClass) {
	m := storagev1.VolumeBindingImmediate
	sc.VolumeBindingMode = &m
}

// TestSharedStorage: without a class that passed the multi-node test, a
// connection holds fleet shares in single-node mode, in the shared class
// else the instance disks', LIMITED on a cluster of several nodes (each
// fleet on one node) with why; a class binding Immediately on several nodes
// is flagged with the fix; where the deep test found two pods cannot mount
// one RWO volume, the copy tier, LIMITED as logins may need repeating.
func TestSharedStorage(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name      string
		objects   []runtime.Object
		rec       fleet.KubeCluster
		multiPod  *bool
		result    fleetgrpc.KubeCheckResult
		mode      string
		detail    string
		limited   string
		multiFlag bool
	}{
		{name: "minikube", objects: []runtime.Object{minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault)},
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, mode: "single-node", detail: "single-node mode: each fleet's instances run together on the node holding its share (standard, Immediate, node-local, not expandable"},
		{name: "a shared class not tested", objects: []runtime.Object{cloudNode("a"), cloudNode("b"), storageClass("gp3", "ebs.csi.aws.com", isDefault, wffc), storageClass("nfs", "nfs.csi.k8s.io", wffc, expandable)},
			rec: fleet.KubeCluster{SharedStorageClass: "nfs"}, result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, mode: "single-node", detail: "nfs is not tested yet for the multi-node test; single-node mode: each fleet's instances run together on the node holding its share (nfs, WFFC, expandable)", limited: LimitedOneNode},
		{name: "a block shared class", objects: []runtime.Object{cloudNode("a"), cloudNode("b"), storageClass("gp3", "ebs.csi.aws.com", isDefault, wffc)},
			rec: fleet.KubeCluster{SharedStorageClass: "gp3"}, result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, mode: "single-node", detail: "gp3 does not give multi-node mode: its volumes mount on one node at a time", limited: LimitedOneNode},
		{name: "Immediate on two nodes", objects: []runtime.Object{cloudNode("a"), cloudNode("b"), storageClass("fast", "ebs.csi.aws.com", isDefault, immediateOne)},
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, mode: "single-node", detail: "binds each volume as soon as it is made", limited: "fleet shares: Immediate binding"},
		{name: "a missing shared class", objects: []runtime.Object{minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault)},
			rec: fleet.KubeCluster{SharedStorageClass: "nope"}, result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, detail: `"nope" for fleet shares does not exist`, limited: "fleet shares unavailable"},
		{name: "the copy tier", objects: []runtime.Object{minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault)},
			multiPod: &no, result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, mode: "copy", detail: "copy tier", limited: LimitedCopyTier},
		{name: "tested", objects: []runtime.Object{minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault)},
			multiPod: &yes, result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, mode: "single-node", multiFlag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, tc.objects...)
			tgt := target(c, tc.rec)
			tgt.MultiPodRWO = tc.multiPod
			res := Full(context.Background(), tgt, nil)
			got := check(t, res, CheckSharedStorage)
			if got.GetResult() != tc.result || !strings.Contains(got.GetDetail(), tc.detail) {
				t.Fatalf("shared_storage %v", got)
			}
			if res.Capabilities.GetShareMode() != tc.mode || res.Capabilities.GetMultiPodRwo() != tc.multiFlag {
				t.Fatalf("share mode %q multi-pod %v", res.Capabilities.GetShareMode(), res.Capabilities.GetMultiPodRwo())
			}
			if tc.limited != "" {
				wantState(t, res, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, tc.limited)
			} else if strings.Contains(res.Message, "fleet shares") || strings.Contains(res.Message, LimitedCopyTier) || strings.Contains(res.Message, LimitedOneNode) {
				t.Fatalf("limited by the share: %q", res.Message)
			}
			if tc.mode == "" && res.Capabilities.GetShareRefused() == "" {
				t.Fatal("no share mode, and no refusal saying why")
			}
		})
	}
}

// sharePodsFake plays the two-pod probe's pods: the first runs on a node,
// the second ends as second says.
type sharePodsFake struct {
	mu      sync.Mutex
	created map[string]*corev1.Pod
	deleted []string
	second  func(*corev1.Pod)
}

func (f *sharePodsFake) install(c *cluster) {
	f.created = map[string]*corev1.Pod{}
	c.cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !strings.HasPrefix(pod.Name, "fleet-probe-rwo-") {
			return false, nil, nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.created[pod.Name] = pod.DeepCopy()
		return true, pod, nil
	})
	c.cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		f.mu.Lock()
		defer f.mu.Unlock()
		pod := f.created[name]
		if pod == nil {
			return false, nil, nil
		}
		out := pod.DeepCopy()
		if strings.HasSuffix(name, "-a") {
			out.Spec.NodeName = "minikube"
			out.Status.Phase = corev1.PodRunning
		} else {
			f.second(out)
		}
		return true, out, nil
	})
	c.cs.PrependReactor("delete", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		if !strings.HasPrefix(name, "fleet-probe-rwo-") {
			return false, nil, nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted = append(f.deleted, a.GetResource().Resource+"/"+name)
		return true, nil, nil
	})
}

// TestDeepSharePods: the deep test mounts one RWO volume from two pods on
// one node (the second pinned to the first's node); a second pod that never
// mounts it puts the connection on the copy tier, the kubelet's mount error
// named; everything the probe made is deleted.
func TestDeepSharePods(t *testing.T) {
	defer func(a, b, c time.Duration) { sharePodsTimeout, sharePodsMount, sharePodsPoll = a, b, c }(sharePodsTimeout, sharePodsMount, sharePodsPoll)
	sharePodsTimeout, sharePodsMount, sharePodsPoll = 5*time.Second, 50*time.Millisecond, time.Millisecond
	for _, tc := range []struct {
		name   string
		second func(*corev1.Pod)
		result fleetgrpc.KubeCheckResult
		mode   string
		rwo    bool
	}{
		{"mounted", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }, fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, "single-node", true},
		{"never mounted", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
		}, fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, "copy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
			f := &sharePodsFake{second: tc.second}
			f.install(c)
			ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev", Namespace: testNS}, Reason: "FailedAttachVolume", Message: "Multi-Attach error for volume pv-1", LastTimestamp: metav1.Now()}
			c.cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.EventList{Items: []corev1.Event{*ev}}, nil
			})
			tgt := target(c, fleet.KubeCluster{})
			r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true}
			r.caps.StorageClasses = []*fleetgrpc.KubeStorageClass{{Name: "standard", IsDefault: true, BindingMode: "Immediate"}}
			r.sharePods(context.Background())
			got := check(t, r.res, CheckSharePods)
			if got.GetResult() != tc.result {
				t.Fatalf("share_pods %v", got)
			}
			if tc.result == fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL && !strings.Contains(got.GetDetail(), "Multi-Attach error") {
				t.Fatalf("a failed probe does not name the kubelet's error: %v", got)
			}
			if r.caps.GetShareMode() != tc.mode || r.res.MultiPodRWO == nil || *r.res.MultiPodRWO != tc.rwo {
				t.Fatalf("share mode %q, multi-pod %v", r.caps.GetShareMode(), r.res.MultiPodRWO)
			}
			var second *corev1.Pod
			for name, p := range f.created {
				if strings.HasSuffix(name, "-b") {
					second = p
				}
			}
			if second == nil || second.Spec.NodeName != "minikube" || second.Spec.Volumes[0].PersistentVolumeClaim == nil {
				t.Fatalf("the second pod %+v is not pinned to the first's node with the volume", second)
			}
			if len(f.deleted) != 3 {
				t.Fatalf("deleted %v, want both pods and the volume", f.deleted)
			}
		})
	}
}

// TestDeepSharePodsLiftsTheCopyTier: a connection an earlier deep test put on
// the copy tier, whose probe now mounts the volume from two pods, is back in
// single-node mode and no longer LIMITED for it.
func TestDeepSharePodsLiftsTheCopyTier(t *testing.T) {
	defer func(a, b, c time.Duration) { sharePodsTimeout, sharePodsMount, sharePodsPoll = a, b, c }(sharePodsTimeout, sharePodsMount, sharePodsPoll)
	sharePodsTimeout, sharePodsMount, sharePodsPoll = 5*time.Second, 50*time.Millisecond, time.Millisecond
	c := newCluster(t, minikubeNode(), storageClass("standard", "k8s.io/minikube-hostpath", isDefault))
	f := &sharePodsFake{second: func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }}
	f.install(c)
	tgt := target(c, fleet.KubeCluster{})
	no := false
	r := &run{t: &tgt, caps: &fleetgrpc.KubeCapabilities{}, nsExists: true, multiPodRWO: &no}
	r.caps.StorageClasses = []*fleetgrpc.KubeStorageClass{{Name: "standard", IsDefault: true, BindingMode: "Immediate"}}
	r.sharedStorage(context.Background())
	if r.caps.GetShareMode() != "copy" || !slices.Contains(r.limited, LimitedCopyTier) {
		t.Fatalf("before the probe: share mode %q, limited %q", r.caps.GetShareMode(), r.limited)
	}
	r.sharePods(context.Background())
	if got := check(t, r.res, CheckSharedStorage); got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || r.caps.GetShareMode() != "single-node" {
		t.Fatalf("after a passing probe: shared_storage %v, share mode %q", got, r.caps.GetShareMode())
	}
	if len(r.limited) != 0 {
		t.Fatalf("after a passing probe the connection is still limited by %q", r.limited)
	}
}
