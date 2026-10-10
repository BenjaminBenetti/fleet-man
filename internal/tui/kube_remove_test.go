package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// stubKubeRemove records RemoveKubeCluster modes and answers with reply.
func stubKubeRemove(t *testing.T, reply func(fleetgrpc.KubeRemoveMode) *fleetgrpc.RemoveKubeClusterReply) *[]fleetgrpc.KubeRemoveMode {
	t.Helper()
	var modes []fleetgrpc.KubeRemoveMode
	orig := removeKubeClusterRemote
	removeKubeClusterRemote = func(id string, mode fleetgrpc.KubeRemoveMode) (*fleetgrpc.RemoveKubeClusterReply, error) {
		modes = append(modes, mode)
		return reply(mode), nil
	}
	t.Cleanup(func() { removeKubeClusterRemote = orig })
	return &modes
}

// openRemove opens the disconnect dialog of the first connection.
func openRemove(t *testing.T, m *model, sp *settingsPage) *kubeRemoveDialog {
	t.Helper()
	sp.cursor = settingsPositionOf(sp, m, settingsItemKubeBase)
	press(m, sp, "l")
	press(m, sp, "l")
	press(m, sp, "enter")
	d, ok := sp.kubeDialog.(*kubeRemoveDialog)
	if !ok {
		t.Fatalf("[ delete ] opened %T", sp.kubeDialog)
	}
	return d
}

// An unused connection disconnects on confirm (REFUSE_IF_USED, so nothing
// that started using it meanwhile is lost) and leaves the list at once.
func TestRemoveUnusedDisconnects(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
	modes := stubKubeRemove(t, func(fleetgrpc.KubeRemoveMode) *fleetgrpc.RemoveKubeClusterReply {
		return &fleetgrpc.RemoveKubeClusterReply{Removed: true}
	})
	openRemove(t, m, sp)
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Disconnect ☸ minikube") || !strings.Contains(out, "No instance or agent uses this cluster") {
		t.Fatalf("dialog:\n%s", out)
	}
	// The daemon's push drops the record before its reply lands.
	removeKubeClusterRemote = func(string, fleetgrpc.KubeRemoveMode) (*fleetgrpc.RemoveKubeClusterReply, error) {
		*modes = append(*modes, fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_REFUSE_IF_USED)
		return &fleetgrpc.RemoveKubeClusterReply{Removed: true}, nil
	}
	cmd := press(m, sp, "enter")
	m.handleKubeClustersMsg(kubeClustersMsg{clusters: &fleetgrpc.KubeClusters{}})
	pumpKube(t, m, cmd)
	if len(*modes) != 1 || (*modes)[0] != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_REFUSE_IF_USED {
		t.Fatalf("modes = %v", *modes)
	}
	if sp.kubeDialog != nil || m.kubeStatus("mk-1") != nil || !strings.Contains(m.message, "Disconnected ☸ minikube") {
		t.Fatalf("dialog %T, still listed %v, message %q", sp.kubeDialog, m.kubeStatus("mk-1") != nil, m.message)
	}
}

// When users appeared between opening and confirming, the daemon refuses
// and the dialog turns into the blockers view instead of removing anything.
func TestRemoveRefusedShowsTheNewBlockers(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
	stubKubeRemove(t, func(fleetgrpc.KubeRemoveMode) *fleetgrpc.RemoveKubeClusterReply {
		return &fleetgrpc.RemoveKubeClusterReply{Users: []*fleetgrpc.KubeClusterUser{{Fleet: "fm", Instance: "new", Status: fleetgrpc.InstanceStatus_INSTANCE_STATUS_CREATING}}}
	})
	d := openRemove(t, m, sp)
	pumpKube(t, m, press(m, sp, "enter"))
	if sp.kubeDialog != d || len(d.instances) != 1 || m.kubeStatus("mk-1") == nil {
		t.Fatal("a refusal must keep the connection and list what uses it")
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "• fm/new creating") || !strings.Contains(out, "[ Destroy 1 instance, then disconnect ]") {
		t.Fatalf("blockers view:\n%s", out)
	}
}

// With instances and an agent on it: the dialog lists them; Destroy is
// refused while the agent targets the cluster (it would make new instances
// there); Forget on a reachable cluster needs the name typed and names the
// agent it leaves behind.
func TestRemoveUsedGuardsDestroyAndForget(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("stg-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY))
	m.st.Fleets["fm"] = &fleet.Fleet{Name: "fm", Instances: []*fleet.Instance{{Name: "beta", KubeCluster: "stg-1", Status: fleet.StatusRunning}},
		Settings: fleet.FleetSettings{Agents: []fleet.Agent{{Name: "nightly", KubeCluster: "stg-1"}}}}
	modes := stubKubeRemove(t, func(fleetgrpc.KubeRemoveMode) *fleetgrpc.RemoveKubeClusterReply {
		return &fleetgrpc.RemoveKubeClusterReply{Removed: true, Warnings: []string{"the instances were left running"}}
	})
	d := openRemove(t, m, sp)
	out := flatText(sp.viewSettings(m))
	for _, want := range []string{"1 instance and 1 agent still use this cluster", "• fm/beta running", "• agent fm/nightly",
		"[ Destroy 1 instance, then disconnect ] 1 agent still targets it", "[ Forget (scale to 0, leave PVCs) ] leaves agent fm/nightly without a target · asks for the name", "[ Cancel ]"} {
		if !strings.Contains(out, want) {
			t.Errorf("dialog missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[ Re-point ]") {
		t.Error("Re-point is for a context that went missing")
	}

	pumpKube(t, m, press(m, sp, "enter")) // Destroy
	if len(*modes) != 0 || !strings.Contains(d.err, "give it another target first") {
		t.Fatalf("Destroy must be refused locally while an agent targets it: modes=%v err=%q", *modes, d.err)
	}
	press(m, sp, "j")
	press(m, sp, "enter") // Forget
	if !d.typing {
		t.Fatal("Forget on a reachable cluster must ask for the name")
	}
	d.input.SetValue("stagin")
	press(m, sp, "enter")
	if len(*modes) != 0 || d.err == "" {
		t.Fatal("a wrong name must not forget it")
	}
	d.input.SetValue("staging")
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*modes) != 1 || (*modes)[0] != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET {
		t.Fatalf("modes = %v", *modes)
	}
	if !strings.Contains(m.message, "Disconnected ☸ staging · the instances were left running") {
		t.Fatalf("message = %q", m.message)
	}
}

// Without agents Destroy runs DESTROY_INSTANCES; an unreachable cluster can
// be forgotten without typing its name; a missing context offers Re-point.
func TestRemoveDestroyForgetUnreachableAndRepoint(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("stg-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE))
	m.st.Fleets["fm"] = &fleet.Fleet{Name: "fm", Instances: []*fleet.Instance{{Name: "beta", KubeCluster: "stg-1", Status: fleet.StatusRunning}}}
	modes := stubKubeRemove(t, func(fleetgrpc.KubeRemoveMode) *fleetgrpc.RemoveKubeClusterReply {
		return &fleetgrpc.RemoveKubeClusterReply{Removed: true}
	})
	d := openRemove(t, m, sp)
	cmd := press(m, sp, "enter")
	if !strings.Contains(flatText(sp.viewSettings(m)), "destroying the instances…") {
		t.Fatal("a destroy in flight must say so")
	}
	pumpKube(t, m, cmd)
	if len(*modes) != 1 || (*modes)[0] != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES {
		t.Fatalf("modes = %v", *modes)
	}

	m.kubeClusters.Clusters = []*fleetgrpc.KubeClusterStatus{kubeSt("stg-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE)}
	d = openRemove(t, m, sp)
	press(m, sp, "j")
	pumpKube(t, m, press(m, sp, "enter"))
	if d.typing || (*modes)[1] != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET {
		t.Fatalf("an unreachable cluster forgets without its name: typing=%v modes=%v", d.typing, *modes)
	}

	m.kubeClusters.Clusters = []*fleetgrpc.KubeClusterStatus{kubeSt("stg-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING)}
	openRemove(t, m, sp)
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Context renamed? Use [ Re-point ] instead.") || !strings.Contains(out, "> [ Re-point ]") {
		t.Fatalf("a missing context must offer Re-point first:\n%s", out)
	}
	stubKubeContexts(t, pickerContexts())
	pumpKube(t, m, press(m, sp, "enter"))
	if p, ok := sp.kubeDialog.(*kubePicker); !ok || p.repoint != "stg-1" {
		t.Fatalf("Re-point opened %T", sp.kubeDialog)
	}
}

// Destroying a cluster's instances can take minutes: esc leaves the dialog
// while fleetd works, and the answer then arrives as a message (and the row
// goes) instead of being lost.
func TestRemoveBusyEscLeavesItToFleetd(t *testing.T) {
	m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
	stubKubeRemove(t, func(fleetgrpc.KubeRemoveMode) *fleetgrpc.RemoveKubeClusterReply {
		return &fleetgrpc.RemoveKubeClusterReply{Removed: true, Warnings: []string{"2 PVCs left"}}
	})
	openRemove(t, m, sp)
	working := press(m, sp, "enter")
	press(m, sp, "j") // ignored while fleetd works
	if _, ok := sp.kubeDialog.(*kubeRemoveDialog); !ok {
		t.Fatal("keys other than esc must wait for fleetd")
	}
	press(m, sp, "esc")
	if sp.kubeDialog != nil || !strings.Contains(m.message, "still disconnecting on fleetd") {
		t.Fatalf("esc must leave the dialog to fleetd (dialog %T, message %q)", sp.kubeDialog, m.message)
	}
	pumpKube(t, m, working)
	if m.kubeStatus("mk-1") != nil || !strings.Contains(m.message, "Disconnected ☸ minikube · 2 PVCs left") {
		t.Fatalf("still listed %v, message %q", m.kubeStatus("mk-1") != nil, m.message)
	}
}
