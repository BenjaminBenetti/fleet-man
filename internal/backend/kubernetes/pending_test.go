//go:build !nok8s

package kubernetes

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// An instance pod the scheduler keeps Pending for want of room on its node
// is worded by what it lacks: the volume attach limit (driver, per node),
// or the CPU or memory the fleet's instances ask for together in
// single-node mode, with the fleet-size cap and the way out; any other
// reason is not.
func TestExplainUnschedulable(t *testing.T) {
	caps := func(nodes int) *fleetgrpc.KubeCapabilities {
		c := &fleetgrpc.KubeCapabilities{
			InstanceStorageClass: "csi-hostpath-sc", SingleNodeFleetCap: 2, SingleNodeFleetCapReason: "attach limit",
			StorageClasses: []*fleetgrpc.KubeStorageClass{{Name: "csi-hostpath-sc", Provisioner: "hostpath.csi.k8s.io"}},
		}
		for i := range nodes {
			c.Nodes = append(c.Nodes, &fleetgrpc.KubeNode{Name: string(rune('a' + i)), AttachLimit: 3})
		}
		c.Nodes = append(c.Nodes, &fleetgrpc.KubeNode{Name: "cp", AttachLimit: 99, Excluded: "taint node-role.kubernetes.io/control-plane:NoSchedule"})
		return c
	}
	const limit = "Unschedulable: 0/1 nodes are available: 1 node(s) exceed max volume count. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod."
	const memory = "Unschedulable: 0/2 nodes are available: 1 Insufficient memory, 1 node(s) didn't match pod affinity rules."
	for _, tc := range []struct {
		name         string
		reason       string
		f            PendingFacts
		note, detail []string
		ok           bool
	}{
		{name: "attach limit, single-node mode", reason: limit, ok: true,
			f:      PendingFacts{Capabilities: caps(1), SingleNode: true, Instances: 3},
			note:   []string{"the node's volume attach limit is reached (3 hostpath.csi.k8s.io volumes per node)", "fleet-size cap 2"},
			detail: []string{"stop another of its instances, or use a larger node", "the scheduler says: " + limit}},
		{name: "attach limit on several nodes", reason: limit, ok: true,
			f:      PendingFacts{Capabilities: caps(2), SingleNode: true, Instances: 3},
			detail: []string{"pick a shared StorageClass that passes the multi-node test"}},
		{name: "attach limit in multi-node mode", reason: limit, ok: true,
			f:      PendingFacts{Capabilities: caps(2), Instances: 3},
			note:   []string{"the node's volume attach limit is reached (3 hostpath.csi.k8s.io volumes per node)"},
			detail: []string{"use larger nodes (they attach more volumes)"}},
		{name: "memory, the fleet together", reason: memory, ok: true,
			f:    PendingFacts{Capabilities: caps(2), Cluster: fleet.KubeCluster{Resources: fleet.KubeResources{MemoryRequest: "3Gi"}}, SingleNode: true, Instances: 3},
			note: []string{"the node lacks the memory the fleet's 3 instances ask for together (1500m CPU, 9Gi memory: 500m and 3Gi each)", "fleet-size cap 2"}},
		{name: "another reason", reason: "Unschedulable: 0/1 nodes are available: 1 node(s) had untolerated taint {x: y}.", f: PendingFacts{Capabilities: caps(1), SingleNode: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note, detail, ok := ExplainUnschedulable(tc.reason, tc.f)
			if ok != tc.ok {
				t.Fatalf("ok = %v (%q)", ok, note)
			}
			for _, w := range tc.note {
				if !strings.Contains(note, w) {
					t.Errorf("note %q lacks %q", note, w)
				}
			}
			for _, w := range tc.detail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			if !tc.f.SingleNode && strings.Contains(note, "cap") {
				t.Errorf("a fleet-size cap outside single-node mode: %q", note)
			}
		})
	}
}
