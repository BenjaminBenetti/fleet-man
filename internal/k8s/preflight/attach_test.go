//go:build !nok8s

package preflight

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// sizedNode is a node with cpu and memory allocatable.
func sizedNode(name, cpu, mem string, opts ...func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}},
		Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{Architecture: "amd64"},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// csiNode is node's CSINode with driver allowing limit volumes (0: no
// limit), reporting topology keys when topo.
func csiNode(node, driver string, limit int32, topo bool) *storagev1.CSINode {
	d := storagev1.CSINodeDriver{Name: driver, NodeID: node}
	if limit > 0 {
		d.Allocatable = &storagev1.VolumeNodeResources{Count: &limit}
	}
	if topo {
		d.TopologyKeys = []string{"topology." + driver + "/node"}
	}
	return &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: node}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{d}}}
}

func csiDriver(name string) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func attachment(node, driver string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-" + node + "-" + driver + "-" + strings.Repeat("x", len(node))},
		Spec:       storagev1.VolumeAttachmentSpec{Attacher: driver, NodeName: node},
		Status:     storagev1.VolumeAttachmentStatus{Attached: true},
	}
}

// TestAttachLimitsAndTheFleetCap: the attach_limits check names the instance
// disks' driver's per-node limit; in single-node mode the fleet-size cap is
// the roomiest node's smaller of capacity (allocatable over the requests)
// and attach room (the limit less what is attached and the share), WARN
// when the attach limit binds first, and the one-node warning carries it.
func TestAttachLimitsAndTheFleetCap(t *testing.T) {
	const hostpath = "hostpath.csi.k8s.io"
	hp := storageClass("csi-hostpath-sc", hostpath, isDefault, wffc)
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		rec     fleet.KubeCluster
		result  fleetgrpc.KubeCheckResult
		detail  string
		cap     int32
		reason  string
		warning string
		// forbid makes listing this resource forbidden.
		forbid string
	}{
		{name: "a low limit binds first",
			objects: []runtime.Object{sizedNode("minikube", "4", "8Gi"), hp, csiDriver(hostpath), csiNode("minikube", hostpath, 3, true)},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, detail: "3 volumes per node (hostpath.csi.k8s.io)\nfleet-size cap (single-node mode): 2 instances per fleet (attach limit)",
			cap: 2, reason: "attach limit"},
		{name: "attached volumes count",
			objects: []runtime.Object{sizedNode("minikube", "4", "8Gi"), hp, csiDriver(hostpath), csiNode("minikube", hostpath, 4, true), attachment("minikube", hostpath)},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, cap: 2, reason: "attach limit, 1 attached"},
		{name: "memory binds before a roomy limit",
			objects: []runtime.Object{sizedNode("minikube", "4", "8Gi"), hp, csiDriver(hostpath), csiNode("minikube", hostpath, 127, true)},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, cap: 4, reason: "node memory; attach limit 127"},
		{name: "the connection's own requests",
			objects: []runtime.Object{sizedNode("minikube", "4", "8Gi"), hp, csiDriver(hostpath), csiNode("minikube", hostpath, 127, true)},
			rec:     fleet.KubeCluster{Resources: fleet.KubeResources{CPURequest: "2", MemoryRequest: "1Gi"}},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, cap: 2, reason: "node CPU; attach limit 127"},
		{name: "a driver without a limit",
			objects: []runtime.Object{sizedNode("minikube", "4", "8Gi"), hp, csiDriver(hostpath), csiNode("minikube", hostpath, 0, true)},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, detail: "hostpath.csi.k8s.io sets no per-node volume limit", cap: 4, reason: "node memory"},
		{name: "CSINodes not readable",
			objects: []runtime.Object{sizedNode("minikube", "4", "8Gi"), hp}, forbid: "csinodes",
			result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, detail: "CSINodes are not readable", cap: 4, reason: "node memory"},
		{name: "several nodes: the roomiest, the one-node warning",
			objects: []runtime.Object{sizedNode("a", "4", "8Gi"), sizedNode("b", "8", "16Gi"), hp, csiDriver(hostpath), csiNode("a", hostpath, 16, true), csiNode("b", hostpath, 16, true)},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, cap: 8, reason: "node memory; attach limit 16; 4 on the smallest node", warning: OneNodeWarning + " (cap 8)"},
		{name: "nodes its pods cannot use do not count",
			objects: []runtime.Object{sizedNode("a", "4", "8Gi"), sizedNode("big", "64", "256Gi", cordon), sizedNode("cp", "64", "256Gi", controlPlane), hp, csiDriver(hostpath), csiNode("a", hostpath, 16, true)},
			result:  fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, cap: 4, reason: "node memory; attach limit 16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, tc.objects...)
			if tc.forbid != "" {
				c.cs.PrependReactor("list", tc.forbid, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: tc.forbid}, "", errors.New("no"))
				})
			}
			res := Full(context.Background(), target(c, tc.rec), nil)
			got := check(t, res, CheckAttachLimits)
			if got.GetResult() != tc.result || !strings.Contains(got.GetDetail(), tc.detail) {
				t.Fatalf("attach_limits %v", got)
			}
			caps := res.Capabilities
			if caps.GetSingleNodeFleetCap() != tc.cap || caps.GetSingleNodeFleetCapReason() != tc.reason {
				t.Fatalf("cap %d (%q), want %d (%q)", caps.GetSingleNodeFleetCap(), caps.GetSingleNodeFleetCapReason(), tc.cap, tc.reason)
			}
			if tc.warning != "" && !strings.Contains(strings.Join(res.Warnings, "\n"), tc.warning) {
				t.Fatalf("warnings %q, want %q", res.Warnings, tc.warning)
			}
			if tc.cap > 0 && !strings.Contains(check(t, res, CheckSharedStorage).GetDetail(), "cap "+FleetCapLine(tc.cap, "")) {
				t.Fatalf("shared_storage does not name the cap: %v", check(t, res, CheckSharedStorage))
			}
			var order []string
			for _, ch := range res.Checks {
				order = append(order, ch.GetName())
			}
			if i := strings.Index(strings.Join(order, ","), CheckSharedStorage+","+CheckAttachLimits); i < 0 {
				t.Fatalf("attach_limits does not follow shared_storage: %v", order)
			}
		})
	}
}

// In multi-node mode the attach limit is per node: no fleet-size cap.
func TestAttachLimitsInMultiNodeMode(t *testing.T) {
	const nfs = "nfs.csi.k8s.io"
	c := newCluster(t, sizedNode("a", "4", "8Gi"), sizedNode("b", "4", "8Gi"),
		storageClass("gp3", "ebs.csi.aws.com", isDefault, wffc), storageClass("nfs", nfs, wffc), csiDriver("ebs.csi.aws.com"),
		csiNode("a", "ebs.csi.aws.com", 25, true), csiNode("b", "ebs.csi.aws.com", 25, true))
	tgt := target(c, fleet.KubeCluster{})
	tgt.StorageCache = OpenStorageCache(filepath.Join(t.TempDir(), "storage.json"))
	stubStorageTests(t, map[string]StorageTest{"nfs": passed})
	res := Full(context.Background(), tgt, nil)
	if res.Capabilities.GetShareMode() != "multi-node" {
		t.Fatalf("share mode %q", res.Capabilities.GetShareMode())
	}
	got := check(t, res, CheckAttachLimits)
	if got.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || !strings.Contains(got.GetDetail(), "25 volumes per node (ebs.csi.aws.com); the scheduler spreads instances") {
		t.Fatalf("attach_limits %v", got)
	}
	if res.Capabilities.GetSingleNodeFleetCap() != 0 {
		t.Fatalf("a cap in multi-node mode: %d", res.Capabilities.GetSingleNodeFleetCap())
	}
}

// A deep test's second look at the share replaces the one-node warning
// (cap and all) instead of adding another.
func TestOneNodeWarningOnce(t *testing.T) {
	c := newCluster(t, sizedNode("a", "4", "8Gi"), sizedNode("b", "4", "8Gi"), storageClass("gp3", "ebs.csi.aws.com", isDefault, wffc))
	r := &run{t: ptrTo(target(c, fleet.KubeCluster{})), caps: &fleetgrpc.KubeCapabilities{}}
	r.full(context.Background())
	r.sharedStorage(context.Background())
	n := 0
	for _, w := range r.res.Warnings {
		if strings.HasPrefix(w, OneNodeWarning) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("one-node warnings %q", r.res.Warnings)
	}
}

func ptrTo[T any](v T) *T { return &v }

func cordon(n *corev1.Node) { n.Spec.Unschedulable = true }

func controlPlane(n *corev1.Node) {
	n.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
}

// The nodes the connection's pods cannot run on say why: cordoned, a node
// selector they miss, a taint not tolerated.
func TestNodeExclusions(t *testing.T) {
	gpu := func(n *corev1.Node) { n.Labels["pool"] = "gpu" }
	for _, tc := range []struct {
		name string
		node *corev1.Node
		rec  fleet.KubeCluster
		want string
	}{
		{name: "usable", node: sizedNode("a", "1", "1Gi")},
		{name: "cordoned", node: sizedNode("a", "1", "1Gi", cordon), want: "cordoned"},
		{name: "selector missed", node: sizedNode("a", "1", "1Gi"), rec: fleet.KubeCluster{NodeSelector: map[string]string{"pool": "gpu"}}, want: "node selector pool=gpu"},
		{name: "selector met", node: sizedNode("a", "1", "1Gi", gpu), rec: fleet.KubeCluster{NodeSelector: map[string]string{"pool": "gpu"}}},
		{name: "tainted", node: sizedNode("a", "1", "1Gi", controlPlane), want: "taint node-role.kubernetes.io/control-plane:NoSchedule"},
		{name: "tolerated by key", node: sizedNode("a", "1", "1Gi", controlPlane),
			rec: fleet.KubeCluster{Tolerations: []fleet.KubeToleration{{Key: "node-role.kubernetes.io/control-plane", Operator: "Exists"}}}},
		{name: "tolerated by everything", node: sizedNode("a", "1", "1Gi", controlPlane),
			rec: fleet.KubeCluster{Tolerations: []fleet.KubeToleration{{Operator: "Exists"}}}},
		{name: "another effect", node: sizedNode("a", "1", "1Gi", controlPlane),
			rec: fleet.KubeCluster{Tolerations: []fleet.KubeToleration{{Key: "node-role.kubernetes.io/control-plane", Operator: "Exists", Effect: "NoExecute"}}}, want: "taint node-role.kubernetes.io/control-plane:NoSchedule"},
		{name: "a value", node: sizedNode("a", "1", "1Gi", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "team", Value: "ml", Effect: corev1.TaintEffectNoExecute}}
		}), rec: fleet.KubeCluster{Tolerations: []fleet.KubeToleration{{Key: "team", Value: "web"}}}, want: "taint team=ml:NoExecute"},
		{name: "PreferNoSchedule does not keep pods off", node: sizedNode("a", "1", "1Gi", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "x", Effect: corev1.TaintEffectPreferNoSchedule}}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &run{t: &Target{Cluster: tc.rec}}
			if got := r.excluded(tc.node); got != tc.want {
				t.Fatalf("excluded = %q, want %q", got, tc.want)
			}
		})
	}
}

// Immediate binding keeps a fleet together where the class's volumes carry
// no topology (its CSI driver reports none), and on a cluster whose other
// nodes the connection's pods cannot use.
func TestImmediateBindingAndTheNodesThatCount(t *testing.T) {
	const rbd = "rbd.csi.ceph.com"
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		refused bool
	}{
		{name: "zonal driver on two nodes", objects: []runtime.Object{sizedNode("a", "4", "8Gi"), sizedNode("b", "4", "8Gi"), storageClass("fast", rbd, isDefault, immediateOne),
			csiDriver(rbd), csiNode("a", rbd, 0, true), csiNode("b", rbd, 0, true)}, refused: true},
		{name: "topology-free driver on two nodes", objects: []runtime.Object{sizedNode("a", "4", "8Gi"), sizedNode("b", "4", "8Gi"), storageClass("fast", rbd, isDefault, immediateOne),
			csiDriver(rbd), csiNode("a", rbd, 0, false), csiNode("b", rbd, 0, false)}},
		{name: "a tainted control plane does not count", objects: []runtime.Object{sizedNode("a", "4", "8Gi"), sizedNode("cp", "4", "8Gi", controlPlane), storageClass("fast", rbd, isDefault, immediateOne)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, tc.objects...)
			res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
			got := check(t, res, CheckSharedStorage)
			refused := strings.Contains(got.GetDetail(), "binds each volume as soon as it is made")
			if refused != tc.refused {
				t.Fatalf("refused %v, want %v: %v", refused, tc.refused, got)
			}
			free := false
			for _, sc := range res.Capabilities.GetStorageClasses() {
				free = free || sc.GetTopologyFree()
			}
			if want := tc.name == "topology-free driver on two nodes"; free != want {
				t.Fatalf("topology_free %v, want %v", free, want)
			}
		})
	}
}

// Each class says what fleet shares on it are expected to do, what it
// means for instance disks, and the large minimum Auto asks consent for.
func TestClassHints(t *testing.T) {
	nfsParams := func(sc *storagev1.StorageClass) { sc.Parameters = map[string]string{"protocol": "nfs"} }
	ap := func(sc *storagev1.StorageClass) { sc.Parameters = map[string]string{"provisioningMode": "efs-ap"} }
	for _, tc := range []struct {
		sc                        *storagev1.StorageClass
		share, disk, largeMinimum string
	}{
		{sc: storageClass("azurefile", "file.csi.azure.com"), share: "SMB Azure Files keeps no owners", disk: "a network file share"},
		{sc: storageClass("azurefile-nfs", "file.csi.azure.com", nfsParams), share: "NFS Azure Files: typically passes", disk: "a network file share"},
		{sc: storageClass("efs-ap", "efs.csi.aws.com", ap), share: "EFS access points give every file one owner", disk: "an NFS class"},
		{sc: storageClass("efs", "efs.csi.aws.com"), share: "statically provisioned volumes typically pass", disk: "an NFS class"},
		{sc: storageClass("filestore", "filestore.csi.storage.gke.io"), share: "Filestore CSI driver must be enabled, and each share is at least 1Ti", disk: "an NFS class", largeMinimum: "1Ti"},
		{sc: storageClass("nfs", "nfs.csi.k8s.io"), share: "NFS: typically passes", disk: "root_squash breaks package installs"},
		{sc: storageClass("cephfs", "cephfs.csi.ceph.com"), share: "CephFS: typically passes", disk: "a network file share"},
		{sc: storageClass("longhorn", "driver.longhorn.io"), share: "Longhorn ReadWriteMany: typically passes"},
		{sc: storageClass("gp3", "ebs.csi.aws.com")},
		{sc: storageClass("local-path", "rancher.io/local-path"), disk: "on one node's disk"},
	} {
		t.Run(tc.sc.Name, func(t *testing.T) {
			ks := ClassifyStorageClass(tc.sc, nil)
			for _, f := range []struct{ what, got, want string }{{"share", ks.GetShareHint(), tc.share}, {"disk", ks.GetDiskHint(), tc.disk}} {
				if (f.want == "") != (f.got == "") || !strings.Contains(f.got, f.want) {
					t.Errorf("%s hint %q, want %q", f.what, f.got, f.want)
				}
			}
			if ks.GetLargeMinimum() != tc.largeMinimum {
				t.Errorf("large minimum %q, want %q", ks.GetLargeMinimum(), tc.largeMinimum)
			}
		})
	}
}

// Listing StorageClasses forbidden: the capabilities say so, so the editor
// takes a typed class name.
func TestStorageClassesUnreadable(t *testing.T) {
	c := newCluster(t, minikubeNode())
	c.cs.PrependReactor("list", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "", errors.New("no"))
	})
	res := Full(context.Background(), target(c, fleet.KubeCluster{}), nil)
	if !res.Capabilities.GetStorageClassesUnreadable() {
		t.Fatal("storage_classes_unreadable not set")
	}
}
