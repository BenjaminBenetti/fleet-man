//go:build !nok8s

package topology

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

func nodeTerm(key string, values ...string) corev1.NodeSelectorTerm {
	return corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values}}}
}

func pvWith(terms ...corev1.NodeSelectorTerm) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{}
	if terms != nil {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: terms}}
	}
	return pv
}

func TestVolumeTerms(t *testing.T) {
	node := nodeTerm(corev1.LabelHostname, "node-a")
	zone := nodeTerm(corev1.LabelTopologyZone, "eu-1a")
	for _, tc := range []struct {
		name string
		pv   *corev1.PersistentVolume
		want []corev1.NodeSelectorTerm
	}{
		{"no volume", nil, nil},
		{"no affinity", pvWith(), nil},
		{"a node-local volume", pvWith(node), []corev1.NodeSelectorTerm{node}},
		{"a zonal disk", pvWith(zone), []corev1.NodeSelectorTerm{zone}},
		{"empty terms are dropped", pvWith(corev1.NodeSelectorTerm{}, zone), []corev1.NodeSelectorTerm{zone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := VolumeTerms(tc.pv)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("VolumeTerms = %+v, want %+v", got, tc.want)
			}
			if len(got) > 0 && tc.pv != nil {
				got[0].MatchExpressions[0].Values[0] = "changed"
				if tc.pv.Spec.NodeAffinity.Required.NodeSelectorTerms[len(tc.pv.Spec.NodeAffinity.Required.NodeSelectorTerms)-1].MatchExpressions[0].Values[0] == "changed" {
					t.Fatal("VolumeTerms shares the volume's terms")
				}
			}
		})
	}
}

func TestPlacementAffinity(t *testing.T) {
	zone := nodeTerm(corev1.LabelTopologyZone, "eu-1a")
	holders := &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{names.LabelShareGroup: "abcdefgh", names.LabelRole: "instance"}},
		TopologyKey:   corev1.LabelHostname,
	}}}
	onZone := &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{zone}}}
	for _, tc := range []struct {
		name   string
		p      Placement
		holder bool
		want   *corev1.Affinity
	}{
		{"single-node holder: the share's zone, beside the running holders", Placement{Mode: SingleNode, Terms: []corev1.NodeSelectorTerm{zone}, ShareGroup: "abcdefgh"}, true, &corev1.Affinity{NodeAffinity: onZone, PodAffinity: holders}},
		{"single-node Job: the share's zone only", Placement{Mode: SingleNode, Terms: []corev1.NodeSelectorTerm{zone}, ShareGroup: "abcdefgh"}, false, &corev1.Affinity{NodeAffinity: onZone}},
		{"single-node before the share binds: beside the holders", Placement{Mode: SingleNode, ShareGroup: "abcdefgh"}, true, &corev1.Affinity{PodAffinity: holders}},
		{"single-node Job before the share binds: nothing", Placement{Mode: SingleNode, ShareGroup: "abcdefgh"}, false, nil},
		{"multi-node: the volume's own topology, no co-location", Placement{Mode: MultiNode, Terms: []corev1.NodeSelectorTerm{zone}, ShareGroup: "abcdefgh"}, true, &corev1.Affinity{NodeAffinity: onZone}},
		{"copy: nothing", Placement{Mode: Copy, Terms: []corev1.NodeSelectorTerm{zone}, ShareGroup: "abcdefgh"}, true, nil},
		{"no mode: nothing", Placement{}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.p.Affinity(tc.holder)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Affinity(%v) =\n%+v\nwant\n%+v", tc.holder, got, tc.want)
			}
		})
	}
}

// TestHolderLabelsMatchTheInstancePods: a pod in single-node mode must match
// its own pod-affinity term, or the first one never schedules (the
// scheduler's self-affinity exception needs it).
func TestHolderLabelsMatchTheInstancePods(t *testing.T) {
	want := map[string]string{names.LabelShareGroup: "abcdefgh", names.LabelRole: string(names.RoleInstance)}
	if got := HolderLabels("abcdefgh"); !reflect.DeepEqual(got, want) {
		t.Fatalf("HolderLabels = %v, want %v", got, want)
	}
}

func TestMerge(t *testing.T) {
	hostA := nodeTerm(corev1.LabelHostname, "a")
	zone1 := nodeTerm(corev1.LabelTopologyZone, "z1")
	zone2 := nodeTerm(corev1.LabelTopologyZone, "z2")
	pool := nodeTerm("pool", "gpu")
	sel := func(terms ...corev1.NodeSelectorTerm) *corev1.Affinity {
		return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms}}}
	}
	and := func(ts ...corev1.NodeSelectorTerm) corev1.NodeSelectorTerm {
		var out corev1.NodeSelectorTerm
		for _, t := range ts {
			out.MatchExpressions = append(out.MatchExpressions, t.MatchExpressions...)
		}
		return out
	}
	if got := Merge(nil, nil); got != nil {
		t.Fatalf("Merge(nil, nil) = %+v", got)
	}
	a := sel(hostA)
	if got := Merge(a, nil); !reflect.DeepEqual(got, a) || got == a {
		t.Fatalf("Merge(a, nil) = %+v (same pointer %v), want a copy of a", got, got == a)
	}
	got := Merge(sel(zone1, zone2), sel(pool))
	want := sel(and(zone1, pool), and(zone2, pool))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge crossed the terms as\n%+v\nwant\n%+v", got, want)
	}
	holders := Placement{Mode: SingleNode, ShareGroup: "abcdefgh"}.Affinity(true)
	got = Merge(sel(hostA), holders)
	if got.NodeAffinity == nil || len(got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) != 1 || got.PodAffinity == nil || len(got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("Merge(node, pod) = %+v, want both", got)
	}
}

func TestImmediateProblem(t *testing.T) {
	pin := func(key string, values ...string) []corev1.TopologySelectorTerm {
		return []corev1.TopologySelectorTerm{{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: key, Values: values}}}}
	}
	for _, tc := range []struct {
		name  string
		c     Class
		nodes int
		ok    bool
	}{
		{"WaitForFirstConsumer", Class{Name: "gp3"}, 5, true},
		{"Immediate on one node (minikube's hostpath)", Class{Name: "standard", Immediate: true, NodeLocal: true}, 1, true},
		{"Immediate, nodes unknown", Class{Name: "standard", Immediate: true}, 0, true},
		{"Immediate on many nodes", Class{Name: "ebs", Immediate: true}, 3, false},
		{"Immediate, node-local, on many nodes", Class{Name: "local", Immediate: true, NodeLocal: true}, 3, false},
		{"Immediate network disk pinned to one zone", Class{Name: "ebs", Immediate: true, AllowedTopologies: pin(corev1.LabelTopologyZone, "eu-1a")}, 3, true},
		{"Immediate network disk pinned to one zone (beta key)", Class{Name: "ebs", Immediate: true, AllowedTopologies: pin(corev1.LabelFailureDomainBetaZone, "eu-1a")}, 3, true},
		{"Immediate network disk in two zones", Class{Name: "ebs", Immediate: true, AllowedTopologies: pin(corev1.LabelTopologyZone, "eu-1a", "eu-1b")}, 3, false},
		{"Immediate node-local pinned to one zone", Class{Name: "local", Immediate: true, NodeLocal: true, AllowedTopologies: pin(corev1.LabelTopologyZone, "eu-1a")}, 3, false},
		{"Immediate node-local pinned to one node", Class{Name: "local", Immediate: true, NodeLocal: true, AllowedTopologies: pin(corev1.LabelHostname, "node-a")}, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why := ImmediateProblem(tc.c, tc.nodes)
			if (why == "") != tc.ok {
				t.Fatalf("ImmediateProblem = %q, want ok=%v", why, tc.ok)
			}
			if why != "" && (!strings.Contains(why, tc.c.Name) || !strings.Contains(why, "Immediate")) {
				t.Fatalf("ImmediateProblem = %q, want it to name the class and its binding mode", why)
			}
		})
	}
}

func TestModes(t *testing.T) {
	for m, shared := range map[Mode]bool{SingleNode: true, MultiNode: true, Copy: false} {
		if !m.Valid() || m.Shared() != shared {
			t.Errorf("%s: Valid %v Shared %v", m, m.Valid(), m.Shared())
		}
	}
	if Mode("").Valid() || Mode("node").Valid() {
		t.Error("an unknown mode is valid")
	}
}
