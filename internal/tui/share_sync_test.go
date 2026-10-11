package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// A running cluster instance's row shows ⟳ syncing beside its cluster while
// its shared folders sync, and "sync paused: <reason>" among its notes
// while a folder cannot sync; nothing while in sync, and nothing on a
// stopped row.
func TestShareSyncRowBadges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  fleet.InstanceStatus
		sync    *fleetgrpc.ShareSyncStatus
		want    []string
		wantNot []string
	}{
		{"in sync", fleet.StatusRunning, &fleetgrpc.ShareSyncStatus{State: fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_IN_SYNC},
			[]string{"☸ minikube"}, []string{kubeSyncGlyph, "sync paused"}},
		{"syncing", fleet.StatusRunning, &fleetgrpc.ShareSyncStatus{State: fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_SYNCING, Pending: 3},
			[]string{"☸ minikube " + kubeSyncGlyph + " syncing"}, []string{"sync paused"}},
		{"paused", fleet.StatusRunning, &fleetgrpc.ShareSyncStatus{State: fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_PAUSED, Reason: "share full"},
			[]string{"running · sync paused: share full"}, []string{kubeSyncGlyph}},
		{"stopped", fleet.StatusStopped, &fleetgrpc.ShareSyncStatus{State: fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_SYNCING},
			nil, []string{kubeSyncGlyph, "sync paused"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("eta", tc.status, ""))
			m.runtime["alpha/eta"] = &fleetgrpc.InstanceRuntime{Fleet: "alpha", Instance: "eta", Kube: &fleetgrpc.KubeInstanceRuntime{}, ShareSync: tc.sync}
			fp.cursor = 0
			line := rowLine(t, m, fp, "eta")
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("row %q lacks %q", line, w)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(line, w) {
					t.Errorf("row %q has %q", line, w)
				}
			}
		})
	}
}

// The fleet options say the same pause under Custom mounts, and a custom
// mount's host folder is a synced copy for a fleet on a cluster.
func TestEditFleetShowsTheSyncOfAFleetOnACluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)
	inst := kubeInst("eta", fleet.StatusRunning, "")
	f := &fleet.Fleet{Name: "alpha", Settings: fleet.FleetSettings{CustomMounts: []string{"/opt/data"}}, Instances: []*fleet.Instance{inst}}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp, runtime: map[string]*fleetgrpc.InstanceRuntime{}}
	m.kubeClusters = &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)}}
	m.runtime["alpha/eta"] = &fleetgrpc.InstanceRuntime{ShareSync: &fleetgrpc.ShareSyncStatus{
		State: fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_PAUSED, Reason: "share full; the daemon host's disk is under 5% free"}}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	navigateToCustomMountRow(t, fp, m, 0)
	view := fp.renderEditFleet(m)
	for _, want := range []string{
		"sync paused: share full",
		"sync paused: the daemon host's disk is under 5% free",
		"host: ~/.fleet/workspaces/alpha/.mnt/opt/data (synced with minikube)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the fleet options lack %q:\n%s", want, view)
		}
	}

	// A fleet with no instance on a cluster: a plain host folder, no pause.
	f.Instances = nil
	view = fp.renderEditFleet(m)
	if strings.Contains(view, "synced with") || strings.Contains(view, "sync paused") {
		t.Errorf("a fleet on no cluster shows the sync:\n%s", view)
	}
}
