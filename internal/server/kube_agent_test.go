//go:build !nok8s

package server

import (
	"context"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const agentTestCluster = "minikube-abc234"

// seedAgentFleet saves fleet alpha with agents and one connection,
// agentTestCluster.
func seedAgentFleet(t *testing.T, agents ...fleet.Agent) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git", Settings: fleet.FleetSettings{Agents: agents}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{{ID: agentTestCluster, Context: "minikube", Namespace: "fleet"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func setAgents(svc *service, agents ...fleet.Agent) error {
	_, err := svc.SetFleetSettings(context.Background(), &fleetgrpc.SetFleetSettingsRequest{
		Fleet:    "alpha",
		Settings: protoconv.FleetSettingsToProto(fleet.FleetSettings{Agents: agents}),
	})
	return err
}

// A Kubernetes agent is saved on a connection the daemon has, and refused
// on one it does not, unless that is the target it already had: an agent
// whose connection was forgotten keeps saving its other fields, but a new
// one, a rename or a change of target must name a live connection.
func TestSetFleetSettingsChecksAgentClusters(t *testing.T) {
	isolateFleetDir(t)
	gone := fleet.Agent{Name: "old", Backend: fleet.BackendKubernetes, KubeCluster: "forgotten-xyz234"}
	seedAgentFleet(t, gone)
	svc := newService()

	live := fleet.Agent{Name: "k", Backend: fleet.BackendKubernetes, KubeCluster: agentTestCluster}
	if err := setAgents(svc, gone, live); err != nil {
		t.Fatalf("an agent on a live connection: %v", err)
	}
	edited := gone
	edited.Command = "claude '${PROMPT}'"
	if err := setAgents(svc, edited, live); err != nil {
		t.Fatalf("editing an agent whose connection is gone: %v", err)
	}

	renamed := edited
	renamed.Name = "renamed"
	newOnMissing := fleet.Agent{Name: "n", Backend: fleet.BackendKubernetes, KubeCluster: "nope-abc234"}
	moved := live
	moved.KubeCluster = "nope-abc234"
	for name, agents := range map[string][]fleet.Agent{
		"a new agent on an unknown connection": {edited, live, newOnMissing},
		"a rename on a forgotten connection":   {renamed, live},
		"a move to an unknown connection":      {edited, moved},
	} {
		err := setAgents(svc, agents...)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: error = %v, want InvalidArgument", name, err)
		}
	}

	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := st.Fleets["alpha"].Settings.Agents
	if len(got) != 2 || got[0].Name != "old" || got[1].DeployTarget() != live.DeployTarget() {
		t.Fatalf("stored agents = %+v, want the last accepted pair", got)
	}
}

// An automation run of a Kubernetes agent is created on the agent's own
// connection (not the remembered default), as an Automated record with its
// key; a run of an agent whose connection is gone is refused before any
// record.
func TestAutomationCreateGoesToTheAgentsCluster(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonIdentity(t)
	seedAgentFleet(t)
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(fleet.BackendDevcontainer)}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan fleet.BackendType, 1)
	orig := jobRunCreate
	jobRunCreate = func(_ context.Context, _, _, _, _ string, _ bool, bt fleet.BackendType) error {
		seen <- bt
		return nil
	}
	t.Cleanup(func() { jobRunCreate = orig })
	svc, _, cleanup := newTestServer(t)
	defer cleanup()

	now := time.Now()
	agent := fleet.Agent{Name: "k", Backend: fleet.BackendKubernetes, KubeCluster: agentTestCluster}
	name, err := createAutomationInstance(svc, "alpha", agent, now)
	if err != nil {
		t.Fatalf("createAutomationInstance: %v", err)
	}
	select {
	case bt := <-seen:
		if bt != fleet.BackendKubernetes {
			t.Fatalf("provisioned on %s, want kubernetes", bt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the create job never ran")
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.Fleets["alpha"].GetInstance(name)
	if err != nil {
		t.Fatal(err)
	}
	if !inst.Automated || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster != agentTestCluster || inst.KubeKey == "" {
		t.Fatalf("run record = %+v, want an Automated kubernetes record on %s with a key", inst, agentTestCluster)
	}

	forgotten := fleet.Agent{Name: "f", Backend: fleet.BackendKubernetes, KubeCluster: "forgotten-xyz234"}
	if _, err := createAutomationInstance(svc, "alpha", forgotten, now); status.Code(err) != codes.NotFound {
		t.Fatalf("a run on a forgotten connection: error = %v, want NotFound", err)
	}
	if st, err = state.Load(); err != nil {
		t.Fatal(err)
	}
	if n := len(st.Fleets["alpha"].Instances); n != 1 {
		t.Fatalf("%d records after a refused run, want 1", n)
	}
}

// The create request of an agent run carries the agent's stored target.
func TestAutomationCreateRequest(t *testing.T) {
	for _, tt := range []struct {
		agent   fleet.Agent
		backend fleetgrpc.BackendType
		cluster string
	}{
		{fleet.Agent{Name: "a"}, fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER, ""},
		{fleet.Agent{Name: "a", Backend: fleet.BackendCoder, KubeCluster: "stray"}, fleetgrpc.BackendType_BACKEND_TYPE_CODER, ""},
		{fleet.Agent{Name: "a", Backend: fleet.BackendKubernetes, KubeCluster: "prod"}, fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES, "prod"},
	} {
		req := automationCreateRequest("alpha", "a-1", tt.agent)
		if req.GetBackend() != tt.backend || req.GetKubeCluster() != tt.cluster || req.GetFleet() != "alpha" || req.GetInstance() != "a-1" {
			t.Errorf("%+v: request %v", tt.agent, req)
		}
	}
}
