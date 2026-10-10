package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestKubeRowNote(t *testing.T) {
	cases := []struct {
		rt   *fleetgrpc.InstanceRuntime
		want string
	}{
		{nil, ""},
		{&fleetgrpc.InstanceRuntime{}, ""},
		{&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{Agent: fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_CONNECTED}}, ""},
		{&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{RestartCause: "node drained"}}, "sessions reset: node drained"},
		{&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{RestartCause: "pod deleted"}}, "sessions reset: pod deleted"},
		{&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{RestartCause: "OOM-killed", SessionsEnded: true}}, "restarted: out of memory, sessions ended"},
	}
	for _, tc := range cases {
		if got := kubeRowNote(tc.rt); got != tc.want {
			t.Errorf("kubeRowNote(%v) = %q, want %q", tc.rt, got, tc.want)
		}
	}
}

func TestViewFleetListShowsSessionsReset(t *testing.T) {
	inst := &fleet.Instance{Name: "epsilon", Status: fleet.StatusRunning, ContainerID: "k8s:c1/ns/fi-alpha-epsilon-abcdefgh@g1", Backend: fleet.BackendKubernetes}
	stopped := &fleet.Instance{Name: "gamma", Status: fleet.StatusStopped, ContainerID: "k8s:c1/ns/fi-alpha-gamma-abcdefgh@g1", Backend: fleet.BackendKubernetes}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowInstance, fleetName: "alpha", instance: stopped},
		{kind: rowSettings},
	}
	m := &model{
		st:     &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst, stopped}}}},
		config: &state.Config{},
		runtime: map[string]*fleetgrpc.InstanceRuntime{
			"alpha/epsilon": {Fleet: "alpha", Instance: "epsilon", Kube: &fleetgrpc.KubeInstanceRuntime{RestartCause: "node drained", RestartHint: "turn on protect_from_drain"}},
			"alpha/gamma":   {Fleet: "alpha", Instance: "gamma", Kube: &fleetgrpc.KubeInstanceRuntime{RestartCause: "crashed"}},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
	}
	view := fp.viewFleetList(m)
	if !strings.Contains(view, "running · sessions reset: node drained") {
		t.Fatalf("the running row has no notice:\n%s", view)
	}
	if strings.Contains(view, "protect_from_drain") {
		t.Fatalf("an unselected row shows the hint:\n%s", view)
	}
	fp.cursor = 1 // the epsilon row
	if view := fp.viewFleetList(m); !strings.Contains(view, "sessions reset: node drained (turn on protect_from_drain)") {
		t.Fatalf("the selected row has no hint:\n%s", view)
	}
	// A stopped row's warning explains it; the notice is for running rows.
	if strings.Contains(view, "sessions reset: crashed") {
		t.Fatalf("a stopped row shows the notice:\n%s", view)
	}
}
