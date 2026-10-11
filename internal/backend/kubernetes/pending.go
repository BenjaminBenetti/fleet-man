//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

// pending.go words why the scheduler keeps an instance pod Pending when the
// reason is the node's room: its CSI driver's volume attach limit (CSINode
// allocatable, the scheduler's NodeVolumeLimits: "node(s) exceed max volume
// count") or its CPU or memory. In single-node mode every instance of a
// fleet, and its share, runs on one node, so the fleet's requests add up
// there and its fleet-size cap (the preflight's single_node_fleet_cap)
// bounds it: the words name the limit, what the fleet asks for together and
// the way out (a shared class that passes the multi-node test spreads the
// fleet across nodes).

// PendingFacts is what explains an unschedulable instance pod.
type PendingFacts struct {
	// Capabilities are the connection's last full preflight's.
	Capabilities *fleetgrpc.KubeCapabilities
	// Cluster is the connection's record (the instances' requests).
	Cluster fleet.KubeCluster
	// SingleNode: the instance's fleet runs together on one node.
	SingleNode bool
	// Instances is how many of the fleet's instances on the connection want
	// to run, this one included (0: unknown).
	Instances int
}

// ExplainUnschedulable rewords reason (the pod's PodScheduled=False
// "Unschedulable: ..." reason) when the node's room is what keeps it
// Pending: note is the row's short form, detail the error's, with what to
// do and the scheduler's own words. ok is false for any other reason.
func ExplainUnschedulable(reason string, f PendingFacts) (note, detail string, ok bool) {
	low := strings.ToLower(reason)
	limit := strings.Contains(low, "max volume count")
	lacks := []string{}
	if strings.Contains(low, "insufficient cpu") {
		lacks = append(lacks, "CPU")
	}
	if strings.Contains(low, "insufficient memory") {
		lacks = append(lacks, "memory")
	}
	if !limit && len(lacks) == 0 {
		return "", "", false
	}
	caps := f.Capabilities
	capText := ""
	if c := caps.GetSingleNodeFleetCap(); f.SingleNode && c > 0 {
		capText = fmt.Sprintf("fleet-size cap %d", c)
	}
	var parts []string
	if limit {
		what := "the node's volume attach limit is reached"
		if l, driver := attachLimitOf(caps); l > 0 {
			what = fmt.Sprintf("the node's volume attach limit is reached (%d %s volumes per node)", l, driver)
		}
		parts = append(parts, what)
	}
	if len(lacks) > 0 {
		what := "the node lacks the " + strings.Join(lacks, " and ") + " it asks for"
		if f.SingleNode && f.Instances > 1 {
			cpu, mem := requestsOf(f.Cluster)
			what = fmt.Sprintf("the node lacks the %s the fleet's %d instances ask for together (%s CPU, %s memory: %s and %s each)",
				strings.Join(lacks, " and "), f.Instances, scaled(cpu, f.Instances, true), scaled(mem, f.Instances, false), cpu, mem)
		}
		parts = append(parts, what)
	}
	if capText != "" {
		parts = append(parts, capText)
	}
	note = strings.Join(parts, "; ")
	how := "stop another instance on its node, or use larger nodes"
	if limit {
		how = "stop another instance on its node, or use larger nodes (they attach more volumes)"
	}
	if f.SingleNode {
		how = "in single-node mode the fleet's instances and its share all run on one node: stop another of its instances"
		if usableNodes(caps) > 1 {
			how += ", or pick a shared StorageClass that passes the multi-node test (the cluster editor's Shared storage) so the fleet's instances spread across nodes"
		} else {
			how += ", or use a larger node"
		}
	}
	detail = note + "; " + how + "; the scheduler says: " + reason
	return note, detail, true
}

// attachLimitOf is the instance disks' attach limit, as the largest a node
// fleet's pods may use allows, and its driver.
func attachLimitOf(caps *fleetgrpc.KubeCapabilities) (int32, string) {
	var limit int32
	for _, n := range caps.GetNodes() {
		if n.GetExcluded() == "" && n.GetAttachLimit() > limit {
			limit = n.GetAttachLimit()
		}
	}
	driver := ""
	for _, sc := range caps.GetStorageClasses() {
		if sc.GetName() == caps.GetInstanceStorageClass() {
			driver = sc.GetProvisioner()
		}
	}
	return limit, driver
}

// requestsOf is an instance's CPU and memory request on the connection.
func requestsOf(c fleet.KubeCluster) (cpu, mem string) {
	return orDefault(c.Resources.CPURequest, fleet.DefaultKubeCPURequest), orDefault(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)
}

// scaled is n times quantity q, as a quantity ("1500m", "6Gi"); q itself
// when it does not parse.
func scaled(q string, n int, milli bool) string {
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return q
	}
	if milli {
		return resource.NewMilliQuantity(v.MilliValue()*int64(n), v.Format).String()
	}
	return resource.NewQuantity(v.Value()*int64(n), v.Format).String()
}

// explainDeadline adds the node's room to an unschedulable deadline of
// instance StatefulSet sts (whose fleet's share is in mode) on connection
// clusterID; other errors pass through.
func (p *Pool) explainDeadline(ctx context.Context, clusterID, sts string, mode topology.Mode, err error) error {
	var de *DeadlineError
	if !errors.As(err, &de) || de.Kind != CodeUnschedulable {
		return err
	}
	facts, ferr := ConnectionFacts(ctx, clusterID)
	if ferr != nil {
		return err
	}
	rec, rerr := p.Record(clusterID)
	if rerr != nil {
		return err
	}
	f := PendingFacts{Capabilities: facts.Capabilities, Cluster: rec, SingleNode: mode == topology.SingleNode}
	if f.SingleNode {
		f.Instances = p.fleetWanting(ctx, clusterID, sts)
	}
	return explainWith(err, f)
}

// explainWith rewords err's unschedulable deadline by f (ExplainUnschedulable);
// other errors pass through.
func explainWith(err error, f PendingFacts) error {
	var de *DeadlineError
	if !errors.As(err, &de) || de.Kind != CodeUnschedulable {
		return err
	}
	if _, detail, ok := ExplainUnschedulable(de.Detail, f); ok {
		de.Detail = detail
	}
	return err
}

// fleetWanting counts the instance StatefulSets of instance sts's fleet on
// connection clusterID that want to run, sts included (0 when they cannot
// be read).
func (p *Pool) fleetWanting(ctx context.Context, clusterID, sts string) int {
	cs, ns, err := p.clientFor(clusterID)
	if err != nil {
		return 0
	}
	set, err := cs.AppsV1().StatefulSets(ns).Get(ctx, sts, metav1.GetOptions{})
	if err != nil || set.Labels[names.LabelShareGroup] == "" {
		return 0
	}
	n, _ := p.groupWanting(ctx, clusterID, set.Labels[names.LabelShareGroup], sts)
	return n
}

// groupWanting counts the instance StatefulSets of the fleet with hash h8
// on connection clusterID that want to run, and reports whether sts is one
// of the fleet's.
func (p *Pool) groupWanting(ctx context.Context, clusterID, h8, sts string) (int, bool) {
	cs, ns, err := p.clientFor(clusterID)
	if err != nil {
		return 0, false
	}
	owner, err := p.owner()
	if err != nil {
		return 0, false
	}
	sel := names.Selector(owner, clusterID) + "," + names.LabelShareGroup + "=" + h8 + "," + names.LabelRole + "=" + string(names.RoleInstance)
	l, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return 0, false
	}
	n, found := 0, false
	for _, s := range l.Items {
		found = found || s.Name == sts
		if s.Spec.Replicas == nil || *s.Spec.Replicas > 0 {
			n++
		}
	}
	return n, found
}

// clientFor is connection clusterID's client and namespace.
func (p *Pool) clientFor(clusterID string) (kclient.Interface, string, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return nil, "", err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, "", err
	}
	return cs, c.Namespace(), nil
}
