package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// unwrapped is a dialog's text with its border, its styling and its line
// wrapping gone, so a sentence the box wrapped reads whole.
func unwrapped(view string) string {
	var words []string
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		l = strings.Trim(l, "│╭╮╰╯─ ")
		if l != "" {
			words = append(words, strings.Fields(l)...)
		}
	}
	return strings.Join(words, " ")
}

// buildkitClusters are a local cluster that runs shared Buildkit, a remote
// one, one whose Allow shared Buildkit is off and one whose admission
// denies the server's pod.
func buildkitClusters() *fleetgrpc.KubeClusters {
	off := false
	return &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{
		{Cluster: &fleetgrpc.KubeCluster{Id: "mini", DisplayName: "minikube"}, Capabilities: &fleetgrpc.KubeCapabilities{LocalCluster: true, BuildkitAdmitted: true}},
		{Cluster: &fleetgrpc.KubeCluster{Id: "eu", DisplayName: "prod-eu"}, Capabilities: &fleetgrpc.KubeCapabilities{BuildkitAdmitted: true}},
		{Cluster: &fleetgrpc.KubeCluster{Id: "us", DisplayName: "prod-us", AllowSharedBuildkit: &off}, Capabilities: &fleetgrpc.KubeCapabilities{BuildkitAdmitted: true}},
		{Cluster: &fleetgrpc.KubeCluster{Id: "base", DisplayName: "baseline"}, Capabilities: &fleetgrpc.KubeCapabilities{
			BuildkitReason: "Shared Buildkit unavailable on this cluster: the namespace's admission denies its pod (violates PodSecurity). Use a sysbox runtime class or a namespace that allows it.",
		}},
	}}
}

func TestBuildkitClusterNote(t *testing.T) {
	m := &model{kubeClusters: buildkitClusters()}
	for id, want := range map[string]string{
		"mini": "",
		"eu":   "prod-eu: shared Buildkit will run in the namespace",
		"us":   "prod-us: Allow shared Buildkit is off there; instances there build without it",
		"base": "baseline: shared Buildkit unavailable: the namespace's admission denies its pod (use a sysbox runtime class)",
		"gone": "",
	} {
		if got := buildkitClusterNote(m.kubeStatus(id)); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}

// TestEditFleetBuildkitRowNamesItsClusters: with shared Buildkit on, the
// Buildkit row notes each cluster the fleet's instances run on that is not
// local (it will run in the namespace), that does not allow it (naming the
// setting) or denies it, once each; with it off, nothing.
func TestEditFleetBuildkitRowNamesItsClusters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)
	f := &fleet.Fleet{Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}, Instances: []*fleet.Instance{
		{Name: "d1", Backend: fleet.BackendDevcontainer},
		{Name: "k1", Backend: fleet.BackendKubernetes, KubeCluster: "mini"},
		{Name: "k2", Backend: fleet.BackendKubernetes, KubeCluster: "eu"},
		{Name: "k3", Backend: fleet.BackendKubernetes, KubeCluster: "us"},
		{Name: "k4", Backend: fleet.BackendKubernetes, KubeCluster: "eu"},
	}}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp, kubeClusters: buildkitClusters()}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.editFleet.cachingExpanded = true

	view := unwrapped(fp.renderEditFleet(m))
	for _, want := range []string{"⚠ prod-eu: shared Buildkit will run in the namespace", "⚠ prod-us: Allow shared Buildkit is off there"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if strings.Count(view, "prod-eu:") != 1 || strings.Contains(view, "minikube:") {
		t.Errorf("each cluster once, the local one not at all:\n%s", view)
	}
	fp.editFleet.buildkitServer = false
	if view := unwrapped(fp.renderEditFleet(m)); strings.Contains(view, "prod-eu:") {
		t.Errorf("notes with Buildkit off:\n%s", view)
	}
}

// TestDeployRowNotesSharedBuildkit: a fleet with shared Buildkit on that is
// deploying to a cluster that is not local is told it will run in the
// namespace, and why where it will not; a fleet without it is told nothing.
func TestDeployRowNotesSharedBuildkit(t *testing.T) {
	for _, tt := range []struct {
		buildkit bool
		cluster  string
		want     string
	}{
		{true, "eu", "⚠ prod-eu: shared Buildkit will run in the namespace"},
		{true, "base", "⚠ baseline: shared Buildkit unavailable"},
		{true, "mini", ""},
		{false, "eu", ""},
	} {
		fp := newFleetPage()
		f := &fleet.Fleet{Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: tt.buildkit}}
		m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp, kubeClusters: buildkitClusters()}
		fp.dlg.fleet = "alpha"
		fp.addInst.target = fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: tt.cluster}
		view := unwrapped(fp.renderAddInstanceDialog(m))
		if tt.want == "" {
			if strings.Contains(view, "Buildkit") {
				t.Errorf("buildkit=%v %s: %s", tt.buildkit, tt.cluster, view)
			}
		} else if !strings.Contains(view, tt.want) {
			t.Errorf("buildkit=%v %s: view lacks %q:\n%s", tt.buildkit, tt.cluster, tt.want, view)
		}
	}
}
