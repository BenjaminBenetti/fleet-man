package tui

import (
	"fmt"
	"slices"
	"strings"
)

// audioUnavailableNote is the Microphone and Audio Output sections' line on
// the instances that can have no audio (InstanceRuntime.kube's
// audio_unavailable), by connection: "no audio on 2 instances (cluster lab:
// no root)". "" when every instance can.
func audioUnavailableNote(m *model) string {
	if m.st == nil {
		return ""
	}
	type group struct{ cluster, reason string }
	counts := map[group]int{}
	for fleetName, f := range m.st.Fleets {
		for _, inst := range f.Instances {
			reason := m.runtime[rtKey(fleetName, inst.Name)].GetKube().GetAudioUnavailable()
			if reason == "" {
				continue
			}
			name := inst.KubeCluster
			if st := m.kubeStatus(inst.KubeCluster); st.GetCluster().GetDisplayName() != "" {
				name = st.GetCluster().GetDisplayName()
			}
			counts[group{name, reason}]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	var parts []string
	for g, n := range counts {
		noun := "instances"
		if n == 1 {
			noun = "instance"
		}
		parts = append(parts, fmt.Sprintf("no audio on %d %s (cluster %s: %s)", n, noun, g.cluster, g.reason))
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}
