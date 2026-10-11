//go:build !nok8s

package preflight

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

// attach.go is the attach_limits check and the fleet-size cap: how many
// instances one fleet fits in single-node mode. Each instance's disk
// attaches to the node its pod runs on, and the CSI driver caps how many of
// its volumes one node takes (CSINode allocatable, which the scheduler's
// NodeVolumeLimits enforces). In single-node mode every disk of a fleet and
// its share attach to one node, so the fleet fits the smaller of that
// node's room for the instances' requests (allocatable CPU and memory) and
// the volumes it can still attach. In multi-node mode the limit is per
// node, and the scheduler spreads the instances.

// CheckAttachLimits is the check of the per-node volume attach limits.
const CheckAttachLimits = "attach_limits"

// nodeRoom is how many of the connection's instances one node fits.
type nodeRoom struct {
	node string
	// fits is the smallest of the bounds known; by names that bound.
	fits int
	by   string
	// limit is the instance disks' driver's attach limit there (0:
	// none or unknown), attached its volumes attached there now.
	limit, attached int32
}

// usableNodes are the nodes the connection's pods may run on (all of them
// when none is: the scheduler's word is still to come).
func (r *run) usableNodes() []*fleetgrpc.KubeNode {
	var out []*fleetgrpc.KubeNode
	for _, n := range r.caps.Nodes {
		if n.GetExcluded() == "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return r.caps.Nodes
	}
	return out
}

// instanceRequests are the CPU (millicores) and memory (bytes) an instance
// asks for on the connection (0 for one that does not parse).
func instanceRequests(c fleet.KubeCluster) (cpu, mem int64) {
	if q, err := resource.ParseQuantity(orDefault(c.Resources.CPURequest, fleet.DefaultKubeCPURequest)); err == nil {
		cpu = q.MilliValue()
	}
	if q, err := resource.ParseQuantity(orDefault(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)); err == nil {
		mem = q.Value()
	}
	return cpu, mem
}

// shareSlot reports whether a fleet's share counts against the instance
// disks' attach limit: it is a volume of their driver on the fleet's node
// (single-node mode).
func (r *run) shareSlot() bool {
	if r.caps.ShareMode != string(topology.SingleNode) || r.rootClass == nil {
		return false
	}
	c, _, ok := r.shareClass()
	return ok && c.GetProvisioner() == r.rootClass.GetProvisioner()
}

// rooms is each usable node's room for one fleet's instances in
// single-node mode (nil when the nodes were not read).
func (r *run) rooms() []nodeRoom {
	cpu, mem := instanceRequests(r.t.Cluster)
	share := int32(0)
	if r.shareSlot() {
		share = 1
	}
	var out []nodeRoom
	for _, n := range r.usableNodes() {
		room := nodeRoom{node: n.GetName(), fits: -1, limit: n.GetAttachLimit(), attached: n.GetAttached()}
		bound := func(fits int64, by string) {
			fits = max(fits, 0)
			if room.fits < 0 || fits < int64(room.fits) {
				room.fits, room.by = int(fits), by
			}
		}
		if q, err := resource.ParseQuantity(n.GetAllocatableMemory()); err == nil && mem > 0 && n.GetAllocatableMemory() != "" {
			bound(q.Value()/mem, "node memory")
		}
		if q, err := resource.ParseQuantity(n.GetAllocatableCpu()); err == nil && cpu > 0 && n.GetAllocatableCpu() != "" {
			bound(q.MilliValue()/cpu, "node CPU")
		}
		if room.limit > 0 {
			bound(int64(room.limit-room.attached-share), "attach limit")
		}
		if room.fits < 0 {
			continue
		}
		out = append(out, room)
	}
	return out
}

// fleetCap is the fleet-size cap of single-node mode: the room of the
// roomiest node, with what bounds it ("node memory; attach limit 127").
// ok is false when nothing could be read.
func (r *run) fleetCap() (fits int, reason string, ok bool) {
	rooms := r.rooms()
	if len(rooms) == 0 {
		return 0, "", false
	}
	best := slices.MaxFunc(rooms, func(a, b nodeRoom) int { return a.fits - b.fits })
	reason = best.by
	if best.limit > 0 && best.by != "attach limit" {
		reason += fmt.Sprintf("; attach limit %d", best.limit)
	}
	if best.attached > 0 {
		reason += fmt.Sprintf(", %d attached", best.attached)
	}
	if least := slices.MinFunc(rooms, func(a, b nodeRoom) int { return a.fits - b.fits }); least.fits < best.fits {
		reason += fmt.Sprintf("; %d on the smallest node", least.fits)
	}
	return best.fits, reason, true
}

// FleetCapLine is how the cap reads: "6 instances per fleet (node memory;
// attach limit 127)".
func FleetCapLine(fits int32, reason string) string {
	line := fmt.Sprintf("%d instances per fleet", fits)
	if fits == 1 {
		line = "1 instance per fleet"
	}
	if reason != "" {
		line += " (" + reason + ")"
	}
	return line
}

// reportAttachLimits reports the instance disks' attach limits per node,
// and in single-node mode the fleet-size cap: a WARN when the attach limit
// bounds a fleet before the nodes' capacity does.
func (r *run) reportAttachLimits() {
	switch {
	case !r.nodesKnown:
		r.skip(CheckAttachLimits, "nodes are not readable (optional): the volumes a node attaches and the fleet-size cap are unknown")
		return
	case r.rootClass == nil:
		r.skip(CheckAttachLimits, "the instance disks' StorageClass is not known: its driver's attach limit is unknown")
		return
	case r.csiLimits == nil:
		r.skip(CheckAttachLimits, "CSINodes are not readable (optional): the volumes a node attaches are unknown")
		return
	}
	driver := r.rootClass.GetProvisioner()
	var limits []int32
	for _, n := range r.usableNodes() {
		if l := n.GetAttachLimit(); l > 0 {
			limits = append(limits, l)
		}
	}
	var detail string
	if len(limits) == 0 {
		detail = driver + " sets no per-node volume limit"
	} else {
		lo, hi := slices.Min(limits), slices.Max(limits)
		detail = fmt.Sprintf("%d volumes per node (%s)", hi, driver)
		if lo != hi {
			detail = fmt.Sprintf("%d–%d volumes per node (%s)", lo, hi, driver)
		}
	}
	single := r.caps.ShareMode == string(topology.SingleNode)
	if !single {
		if len(limits) > 0 && r.multiNode {
			detail += "; the scheduler spreads instances across the nodes"
		}
		r.pass(CheckAttachLimits, detail)
		return
	}
	fits, reason, ok := r.fleetCap()
	if !ok {
		r.pass(CheckAttachLimits, detail)
		return
	}
	detail += "\nfleet-size cap (single-node mode): " + FleetCapLine(int32(fits), reason)
	if fits < 1 {
		r.warn(CheckAttachLimits, detail+": no node has room for one more of a fleet's instances", r.capFix())
		return
	}
	if strings.HasPrefix(reason, "attach limit") {
		r.warn(CheckAttachLimits, detail+": the volume attach limit bounds a fleet before node capacity does", r.capFix())
		return
	}
	r.pass(CheckAttachLimits, detail)
}

// capFix is the remedy for a tight fleet-size cap.
func (r *run) capFix() string {
	if r.multiNode {
		return "a shared StorageClass that passes the multi-node test lets a fleet's instances (and their disks) spread across nodes; or larger nodes, which attach more volumes"
	}
	return "larger nodes, which attach more volumes, or fewer instances per fleet"
}
