//go:build !nok8s

// Package topology places a fleet's pods on a connection by its share: the
// volume the fleet's agent dirs, shared files and custom mounts live on.
//
// In single-node mode (no StorageClass of the connection passed the
// multi-node share test) every pod of a fleet runs on the node holding its
// share, as every container of a fleet does on one docker host. The share's
// bound volume names where it can be used (spec.nodeAffinity: one node for a
// node-local volume, one zone for a network disk); that becomes a required
// node affinity on every pod and Job of the fleet, so an instance disk that
// binds when its first pod is scheduled (WaitForFirstConsumer) binds beside
// the share even while no other instance runs. A pod that mounts the share
// also goes on the node of the running pods that mount it (a required pod
// affinity on the node, with the scheduler's self-affinity exception for
// the first one): a zonal disk attaches to one node at a time.
//
// In multi-node mode (the share's class passed the multi-node storage test)
// only the share volume's own topology applies, and in copy mode (one RWO volume
// cannot be mounted by two pods on a node) there is no shared volume at
// all: each instance keeps its own copy on its disk.
package topology

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// Mode is how a fleet's share is held on a connection
// (KubeCapabilities.share_mode).
type Mode string

const (
	// SingleNode: one volume, all of the fleet's pods on the node holding
	// it.
	SingleNode Mode = "single-node"
	// MultiNode: one volume of a class that passed the multi-node test,
	// mounted from any node.
	MultiNode Mode = "multi-node"
	// Copy: no shared volume; each instance's copy is on its own disk.
	Copy Mode = "copy"
)

// Valid reports whether m is a mode.
func (m Mode) Valid() bool { return m == SingleNode || m == MultiNode || m == Copy }

// Shared reports whether the mode has a share volume the pods mount.
func (m Mode) Shared() bool { return m == SingleNode || m == MultiNode }

// VolumeTerms is the node affinity of pv: the node selector terms (any one
// of them) of the nodes the volume can be used from. nil means every node.
func VolumeTerms(pv *corev1.PersistentVolume) []corev1.NodeSelectorTerm {
	if pv == nil || pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return nil
	}
	var out []corev1.NodeSelectorTerm
	for _, t := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
			// An empty term matches no node; the API server refuses one
			// anyway.
			continue
		}
		out = append(out, *t.DeepCopy())
	}
	return out
}

// Placement is where a fleet's pods go on a connection.
type Placement struct {
	Mode Mode
	// Terms are the bound share volume's node affinity (VolumeTerms): in
	// single-node mode required on every pod of the fleet, in multi-node mode
	// what the volume itself pins. nil before the share is bound, and for a
	// volume every node reaches.
	Terms []corev1.NodeSelectorTerm
	// ShareGroup is the fleet's h8 on the connection (names.FleetHash): its
	// instance pods carry it as LabelShareGroup.
	ShareGroup string
}

// HolderLabels select the pods that hold a fleet's share in single-node
// mode: its instance pods. A Job that mounts it (the first instance's seed,
// which binds it) is not one: it runs before any other pod of the fleet and
// is gone once the share is bound.
func HolderLabels(shareGroup string) map[string]string {
	return map[string]string{names.LabelShareGroup: shareGroup, names.LabelRole: string(names.RoleInstance)}
}

// Affinity is the affinity one of the fleet's pods takes; holder says
// whether the pod mounts the share (its instance pods do), which in
// single-node mode puts it beside the running holders. nil when the
// placement adds nothing.
func (p Placement) Affinity(holder bool) *corev1.Affinity {
	a := &corev1.Affinity{}
	if len(p.Terms) > 0 && p.Mode.Shared() {
		a.NodeAffinity = &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: deepTerms(p.Terms),
		}}
	}
	if holder && p.Mode == SingleNode && p.ShareGroup != "" {
		a.PodAffinity = &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: HolderLabels(p.ShareGroup)},
			TopologyKey:   corev1.LabelHostname,
		}}}
	}
	if a.NodeAffinity == nil && a.PodAffinity == nil {
		return nil
	}
	return a
}

// Merge is the affinity of both a and b: a pod placed by it satisfies each.
// Required node terms are crossed (each term of a with each of b, as the
// terms of one are ORed and a term's requirements ANDed), the rest added.
// Either may be nil; the result shares nothing with them.
func Merge(a, b *corev1.Affinity) *corev1.Affinity {
	switch {
	case a == nil:
		return b.DeepCopy()
	case b == nil:
		return a.DeepCopy()
	}
	out := a.DeepCopy()
	bb := b.DeepCopy()
	if bb.NodeAffinity != nil {
		if out.NodeAffinity == nil {
			out.NodeAffinity = &corev1.NodeAffinity{}
		}
		out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = crossSelectors(out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution, bb.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		out.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(out.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution, bb.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution...)
	}
	if bb.PodAffinity != nil {
		if out.PodAffinity == nil {
			out.PodAffinity = &corev1.PodAffinity{}
		}
		out.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(out.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, bb.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
		out.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(out.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution, bb.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution...)
	}
	if bb.PodAntiAffinity != nil {
		if out.PodAntiAffinity == nil {
			out.PodAntiAffinity = &corev1.PodAntiAffinity{}
		}
		out.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(out.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, bb.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
		out.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(out.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, bb.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution...)
	}
	return out
}

// crossSelectors is the node selector both a and b require (either may be
// nil: no requirement).
func crossSelectors(a, b *corev1.NodeSelector) *corev1.NodeSelector {
	switch {
	case b == nil || len(b.NodeSelectorTerms) == 0:
		return a
	case a == nil || len(a.NodeSelectorTerms) == 0:
		return b
	}
	out := &corev1.NodeSelector{}
	for _, ta := range a.NodeSelectorTerms {
		for _, tb := range b.NodeSelectorTerms {
			t := *ta.DeepCopy()
			t.MatchExpressions = append(t.MatchExpressions, tb.DeepCopy().MatchExpressions...)
			t.MatchFields = append(t.MatchFields, tb.DeepCopy().MatchFields...)
			out.NodeSelectorTerms = append(out.NodeSelectorTerms, t)
		}
	}
	return out
}

func deepTerms(in []corev1.NodeSelectorTerm) []corev1.NodeSelectorTerm {
	out := make([]corev1.NodeSelectorTerm, len(in))
	for i := range in {
		out[i] = *in[i].DeepCopy()
	}
	return out
}

// The topology keys a StorageClass's allowedTopologies pin a zone or a node
// by.
var (
	zoneKeys = []string{corev1.LabelTopologyZone, corev1.LabelFailureDomainBetaZone}
	nodeKeys = []string{corev1.LabelHostname}
)

// Class is what binding checks need of a StorageClass.
type Class struct {
	Name string
	// Immediate: the class binds a volume when its claim is made, not when
	// its first pod is scheduled.
	Immediate bool
	// NodeLocal: its volumes are on one node's disk (local-path, hostpath).
	NodeLocal bool
	// AllowedTopologies are the class's, as it lists them.
	AllowedTopologies []corev1.TopologySelectorTerm
}

// ImmediateProblem says why volumes of class c cannot keep a fleet on one
// node of a cluster of nodes nodes ("" when they can): a class that binds
// Immediately places each instance disk and the share on its own, so they
// can land on different nodes (or zones) and a pod needing two of them never
// starts. It can on a cluster of one node, and with allowedTopologies
// pinning one node, or one zone for a network volume (which attaches to any
// node in it).
func ImmediateProblem(c Class, nodes int) string {
	if !c.Immediate || nodes <= 1 {
		return ""
	}
	if pinsOne(c.AllowedTopologies, nodeKeys) || (!c.NodeLocal && pinsOne(c.AllowedTopologies, zoneKeys)) {
		return ""
	}
	where := "zone"
	if c.NodeLocal {
		where = "node"
	}
	return fmt.Sprintf("storage class %s binds each volume as soon as it is made (volumeBindingMode Immediate), so on a cluster of %d nodes an instance's disk and its fleet's share can land on different %ss, and its pod never starts", c.Name, nodes, where)
}

// ImmediateFix is what to do about an ImmediateProblem.
const ImmediateFix = "use a StorageClass with volumeBindingMode WaitForFirstConsumer for instance disks and fleet shares, or pin the class to one zone (allowedTopologies)"

// pinsOne reports whether terms allow exactly one value of one of keys:
// a single term whose only expression is one of keys with one value.
func pinsOne(terms []corev1.TopologySelectorTerm, keys []string) bool {
	if len(terms) != 1 || len(terms[0].MatchLabelExpressions) != 1 {
		return false
	}
	e := terms[0].MatchLabelExpressions[0]
	return slices.Contains(keys, e.Key) && len(e.Values) == 1 && strings.TrimSpace(e.Values[0]) != ""
}
