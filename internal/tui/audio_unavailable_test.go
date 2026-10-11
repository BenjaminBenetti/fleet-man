package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The Microphone and Audio Output sections say how many instances can have
// no audio, and on which cluster why, from the daemon's pushed runtime: a
// remote TUI shows it as a local one does.
func TestSettingsSayWhichInstancesHaveNoAudio(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings.Enabled = true
	m.config.OutputSettings.Enabled = true
	kube := func(name string) *fleet.Instance {
		return &fleet.Instance{Name: name, Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes, KubeCluster: "c-lab"}
	}
	m.st = &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{kube("a"), kube("b"), kube("c"), {Name: "local", Status: fleet.StatusRunning}}}}}
	m.kubeClusters = &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{{Cluster: &fleetgrpc.KubeCluster{Id: "c-lab", DisplayName: "lab"}}}}
	if note := audioUnavailableNote(m); note != "" {
		t.Fatalf("note %q with every instance able to have audio", note)
	}
	m.runtime = map[string]*fleetgrpc.InstanceRuntime{
		rtKey("alpha", "a"): {Kube: &fleetgrpc.KubeInstanceRuntime{AudioUnavailable: "no root"}},
		rtKey("alpha", "b"): {Kube: &fleetgrpc.KubeInstanceRuntime{AudioUnavailable: "no root"}},
		rtKey("alpha", "c"): {Kube: &fleetgrpc.KubeInstanceRuntime{}},
	}
	want := "no audio on 2 instances (cluster lab: no root)"
	if note := audioUnavailableNote(m); note != want {
		t.Fatalf("note %q, want %q", note, want)
	}
	m.width, m.height = 140, 200
	view := sp.View(m)
	if got := strings.Count(view, want); got != 2 {
		t.Fatalf("the note shows %d times on the Settings page, want under the Microphone and the Audio Output status:\n%s", got, view)
	}
}
