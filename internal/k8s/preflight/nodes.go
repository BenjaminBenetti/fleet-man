//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

func (r *run) nodes(ctx context.Context) {
	r.running(CheckNodes)
	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.CoreV1().Nodes().List(cctx, metav1.ListOptions{})
	if err != nil {
		r.caps.PrivilegedAllowed = r.t.Cluster.AllowsPrivileged(false)
		if apierrors.IsForbidden(err) {
			r.skip(CheckNodes, "nodes are not readable (optional): architectures, capacity and local-cluster detection unknown, and a fleet on one node is not moved off a node being drained")
			return
		}
		r.warn(CheckNodes, "listing nodes: "+Message(err), "")
		return
	}
	r.multiNode, r.nodesKnown = len(list.Items) > 1, true
	r.providers = map[string]string{}
	r.csiLimits, r.csiTopology = r.attachLimits(ctx)
	archs := map[string]bool{}
	cordoned := 0
	facts := make([]localcluster.Node, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		facts = append(facts, localcluster.Node{Name: n.Name, Labels: n.Labels, ProviderID: n.Spec.ProviderID})
		r.providers[n.Name] = n.Spec.ProviderID
		arch := n.Status.NodeInfo.Architecture
		if arch == "" {
			arch = n.Labels[corev1.LabelArchStable]
		}
		if arch != "" {
			archs[arch] = true
		}
		if n.Spec.Unschedulable {
			cordoned++
		}
		kn := &fleetgrpc.KubeNode{Name: n.Name, Arch: arch, Cordoned: n.Spec.Unschedulable, Excluded: r.excluded(n)}
		if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			kn.AllocatableCpu = q.String()
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			kn.AllocatableMemory = q.String()
		}
		r.caps.Nodes = append(r.caps.Nodes, kn)
	}
	local := localcluster.Detect(facts)
	r.res.Local, r.res.Platform = local, nodePlatform(list.Items)
	r.caps.NodeArchs = sortedKeys(archs)
	r.caps.LocalCluster = local.Local()
	r.caps.Autopilot = gkeAutopilot(list.Items)
	r.caps.PrivilegedAllowed = r.t.Cluster.AllowsPrivileged(local.Local())
	sort.Slice(r.caps.Nodes, func(i, j int) bool { return r.caps.Nodes[i].Name < r.caps.Nodes[j].Name })

	var archParts []string
	for _, a := range r.caps.NodeArchs {
		count := 0
		for _, n := range r.caps.Nodes {
			if n.Arch == a {
				count++
			}
		}
		archParts = append(archParts, fmt.Sprintf("%s×%d", a, count))
	}
	detail := fmt.Sprintf("%d node", len(list.Items))
	if len(list.Items) != 1 {
		detail += "s"
	}
	if len(archParts) > 0 {
		detail += " · " + strings.Join(archParts, " ")
	}
	if local.Local() {
		detail += " · local cluster (" + local.String() + ")"
	}
	if r.caps.Autopilot {
		detail += " · GKE Autopilot"
	}
	if cordoned > 0 {
		detail += fmt.Sprintf(" · %d cordoned", cordoned)
	}
	if len(list.Items) == 0 {
		r.warn(CheckNodes, "no nodes", "")
		return
	}
	r.pass(CheckNodes, detail)
}

// gkeAutopilot reports nodes that are all GKE Autopilot's: Compute Engine
// instances (a gce:// provider id) named gk3-..., as Autopilot names its
// nodes (a Standard cluster's are gke-...). Nodes not readable are not
// known to be.
func gkeAutopilot(nodes []corev1.Node) bool {
	for i := range nodes {
		if !strings.HasPrefix(nodes[i].Name, "gk3-") || !strings.HasPrefix(nodes[i].Spec.ProviderID, "gce://") {
			return false
		}
	}
	return len(nodes) > 0
}

// attachLimits reads each node's per-driver CSI volume limits (CSINode
// allocatable): node -> driver -> count, and whether each driver reports a
// topology on some node (its volumes are then placed by it): driver ->
// true. Optional: an unreadable CSINode list leaves both nil (unknown).
func (r *run) attachLimits(ctx context.Context) (map[string]map[string]int32, map[string]bool) {
	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.StorageV1().CSINodes().List(cctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil
	}
	limits := map[string]map[string]int32{}
	topo := map[string]bool{}
	for _, n := range list.Items {
		for _, d := range n.Spec.Drivers {
			topo[d.Name] = topo[d.Name] || len(d.TopologyKeys) > 0
			if d.Allocatable == nil || d.Allocatable.Count == nil {
				continue
			}
			if limits[n.Name] == nil {
				limits[n.Name] = map[string]int32{}
			}
			limits[n.Name][d.Name] = *d.Allocatable.Count
		}
	}
	return limits, topo
}

// excluded says why the connection's pods cannot run on node n ("" when
// they can): cordoned, not matched by its node selector, or tainted with a
// NoSchedule or NoExecute taint its tolerations do not cover.
func (r *run) excluded(n *corev1.Node) string {
	if n.Spec.Unschedulable {
		return "cordoned"
	}
	for _, k := range sortedKeys(r.t.Cluster.NodeSelector) {
		if v, ok := n.Labels[k]; !ok || v != r.t.Cluster.NodeSelector[k] {
			return "node selector " + k + "=" + r.t.Cluster.NodeSelector[k]
		}
	}
	for _, taint := range n.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !tolerated(r.t.Cluster.Tolerations, taint) {
			what := taint.Key
			if taint.Value != "" {
				what += "=" + taint.Value
			}
			return "taint " + what + ":" + string(taint.Effect)
		}
	}
	return ""
}

// tolerated reports whether one of tolerations covers taint, as the
// scheduler matches them: the key (any with an empty key and Exists), the
// value (Equal, the default operator) and the effect (any when empty).
func tolerated(tolerations []fleet.KubeToleration, taint corev1.Taint) bool {
	for _, t := range tolerations {
		if t.Effect != "" && t.Effect != string(taint.Effect) {
			continue
		}
		switch {
		case t.Key == "" && t.Operator == string(corev1.TolerationOpExists):
			return true
		case t.Key != taint.Key:
			continue
		case t.Operator == string(corev1.TolerationOpExists):
			return true
		case t.Value == taint.Value:
			return true
		}
	}
	return false
}

// fillAttachLimits records, on each node, the attach limit of driver (the
// instance disks' provisioner) and how many of its volumes are attached
// there. VolumeAttachments are optional reads too.
func (r *run) fillAttachLimits(ctx context.Context, driver string) {
	if driver == "" || len(r.caps.Nodes) == 0 {
		return
	}
	attached := map[string]int32{}
	cctx, cancel := call(ctx)
	defer cancel()
	if list, err := r.t.Client.StorageV1().VolumeAttachments().List(cctx, metav1.ListOptions{}); err == nil {
		for _, va := range list.Items {
			if va.Spec.Attacher == driver && va.Status.Attached {
				attached[va.Spec.NodeName]++
			}
		}
	}
	for _, n := range r.caps.Nodes {
		n.AttachLimit = r.csiLimits[n.Name][driver]
		n.Attached = attached[n.Name]
	}
}

func (r *run) metrics(ctx context.Context) {
	r.running(CheckMetrics)
	var err error
	if r.t.Raw != nil {
		// Discovery's own call takes no context and the client sets no
		// timeout: a stalled API server would hold the whole run (and the
		// connection's Lease renewals) for good.
		cctx, cancel := call(ctx)
		_, err = r.t.Raw(cctx, "/apis/metrics.k8s.io/v1beta1")
		cancel()
	} else {
		_, err = r.t.Client.Discovery().ServerResourcesForGroupVersion("metrics.k8s.io/v1beta1")
	}
	if err != nil {
		r.pass(CheckMetrics, "metrics API not served (stats come from the instance agent)")
		return
	}
	r.caps.MetricsAvailable = true
	r.pass(CheckMetrics, "metrics.k8s.io available")
}

func (r *run) runtimeClasses(ctx context.Context) {
	want := r.t.Cluster.RuntimeClass
	r.running(CheckRuntimeClasses)
	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.NodeV1().RuntimeClasses().List(cctx, metav1.ListOptions{})
	if err != nil {
		if want != "" {
			r.warn(CheckRuntimeClasses, fmt.Sprintf("runtime classes are not readable; using %q as set", want), "")
			return
		}
		r.skip(CheckRuntimeClasses, "runtime classes are not readable (optional)")
		return
	}
	found := false
	for _, rc := range list.Items {
		r.caps.RuntimeClasses = append(r.caps.RuntimeClasses, rc.Name)
		if rc.Name == want {
			found = true
		}
	}
	sort.Strings(r.caps.RuntimeClasses)
	switch {
	case want != "" && !found:
		msg := fmt.Sprintf("runtime class %q does not exist", want)
		r.fail(CheckRuntimeClasses, msg, "pick one the cluster offers, or clear the setting")
		r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg)
	case len(r.caps.RuntimeClasses) == 0:
		r.pass(CheckRuntimeClasses, "none (the node's default runtime)")
	default:
		detail := strings.Join(r.caps.RuntimeClasses, ", ")
		if want != "" {
			detail = want + " (of " + detail + ")"
		}
		r.pass(CheckRuntimeClasses, detail)
	}
}
