package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	interruptedErr = "kubernetes:interrupted: interrupted by daemon restart during create (at step seed)"
	oomParkedErr   = "kubernetes:oomkilled: the dev container kept running out of memory (memory limit 1Gi), 3 restarts within 5m0s. Parked as stopped: raise memory_limit in the cluster's resources, then start it again."
)

// kubeRowModel is a fleet page with one fleet of the given instances, the
// connection mk-1 ("minikube") in state, and the cursor on the first
// instance row.
func kubeRowModel(t *testing.T, clusterState fleetgrpc.KubeClusterState, insts ...*fleet.Instance) (*model, *fleetPage) {
	t.Helper()
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvServer, "")
	fp := newFleetPage()
	m := &model{
		st:           &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: insts}}},
		config:       state.DefaultConfig(),
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
		currentPage:  fp,
		spinner:      spinner.New(),
		runtime:      map[string]*fleetgrpc.InstanceRuntime{},
		width:        160,
		height:       50,
	}
	m.kubeClusters = &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{kubeSt("mk-1", "minikube", clusterState)}}
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.kind == rowInstance {
			fp.cursor = i
			break
		}
	}
	return m, fp
}

func kubeInst(name string, status fleet.InstanceStatus, errMsg string) *fleet.Instance {
	return &fleet.Instance{Name: name, Status: status, Backend: fleet.BackendKubernetes, KubeCluster: "mk-1",
		KubeKey: "abcdefghijkl", ContainerID: "k8s:mk-1/fleet/fi-alpha-" + name + "-abcdefgh@g1", Error: errMsg}
}

// rowLine is the rendered row of instance name, as plain text.
func rowLine(t *testing.T, m *model, fp *fleetPage, name string) string {
	t.Helper()
	for _, line := range strings.Split(fp.viewFleetList(m), "\n") {
		plain := flatText(line)
		if strings.Contains(plain, " "+name+" ") {
			return plain
		}
	}
	t.Fatalf("no row for %s:\n%s", name, fp.viewFleetList(m))
	return ""
}

// A running cluster instance's row names its connection and says what its
// runtime and the connection's health say: the pod, the agent, the cluster.
func TestKubeRunningRowNotes(t *testing.T) {
	stats := &fleetgrpc.ContainerStats{CpuMillicores: 120, MemoryMb: 512}
	cases := []struct {
		name     string
		cluster  fleetgrpc.KubeClusterState
		rt       *fleetgrpc.InstanceRuntime
		want     []string
		wantNot  []string
		unknownA bool
	}{
		{"healthy", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY,
			&fleetgrpc.InstanceRuntime{AgentActivity: fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING, AgentTool: fleetgrpc.AgentTool_AGENT_TOOL_CLAUDE, Stats: stats,
				Kube: &fleetgrpc.KubeInstanceRuntime{Agent: fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_CONNECTED}},
			[]string{"running ⏸ Claude Code", "120 mcpu", "☸ minikube"}, []string{"agent ?", "·", "✕"}, false},
		{"reconnecting", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY,
			&fleetgrpc.InstanceRuntime{Stats: stats, Kube: &fleetgrpc.KubeInstanceRuntime{Agent: fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_RECONNECTING}},
			[]string{"running · agent reconnecting", "☸ minikube"}, []string{"idle", "agent ?"}, false},
		{"pod not ready", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY,
			&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{PodReason: "ImagePullBackOff"}},
			[]string{"running · pod: ImagePullBackOff"}, nil, false},
		{"moving", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY,
			&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{NodeCordonedMoving: true}},
			[]string{"running · node cordoned: fleet moving"}, nil, false},
		{"tampered", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY,
			&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{PodReason: "kubernetes:tampered"}},
			[]string{"running · pod tampered with: refused"}, nil, false},
		{"cluster unreachable", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE,
			&fleetgrpc.InstanceRuntime{AgentActivity: fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING, AgentTool: fleetgrpc.AgentTool_AGENT_TOOL_CLAUDE, Stats: stats,
				Kube: &fleetgrpc.KubeInstanceRuntime{Agent: fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_RECONNECTING}},
			[]string{"running · cluster unreachable · agent ?", "☸ minikube ✕"}, []string{"mcpu", "agent reconnecting", "Claude", "working"}, true},
		{"login expired", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED,
			&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{}},
			[]string{"running · cluster login expired · agent ?", "☸ minikube ✕"}, []string{"idle"}, true},
		{"captures failing", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY,
			&fleetgrpc.InstanceRuntime{AgentActivity: fleetgrpc.AgentActivity_AGENT_ACTIVITY_UNKNOWN, Stats: stats, Kube: &fleetgrpc.KubeInstanceRuntime{}},
			[]string{"running · agent ?"}, []string{"mcpu", "idle"}, true},
		{"restart and unreachable", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE,
			&fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{PodReason: "cluster unreachable", RestartCause: "node lost"}},
			[]string{"running · cluster unreachable · sessions reset: node lost · agent ?"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, fp := kubeRowModel(t, tc.cluster, kubeInst("eta", fleet.StatusRunning, ""))
			tc.rt.Fleet, tc.rt.Instance = "alpha", "eta"
			m.runtime["alpha/eta"] = tc.rt
			fp.cursor = 0 // not selected: no hints
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
			if _, unknown := m.kubeRunningNotes(m.st.Fleets["alpha"].Instances[0], tc.rt); unknown != tc.unknownA {
				t.Errorf("agent unknown = %v, want %v", unknown, tc.unknownA)
			}
		})
	}
}

// The unknown agent state is drawn on every backend's rows, not only on a
// cluster's.
func TestUnknownAgentOnADockerRow(t *testing.T) {
	inst := &fleet.Instance{Name: "alpha1", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "c0ffee"}
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, inst)
	m.runtime["alpha/alpha1"] = &fleetgrpc.InstanceRuntime{AgentActivity: fleetgrpc.AgentActivity_AGENT_ACTIVITY_UNKNOWN,
		Stats: &fleetgrpc.ContainerStats{CpuMillicores: 3, MemoryMb: 4}}
	line := rowLine(t, m, fp, "alpha1")
	if !strings.Contains(line, "running · agent ?") || strings.Contains(line, "mcpu") || strings.Contains(line, "idle") {
		t.Fatalf("row %q, want running · agent ? and no stats", line)
	}
	if !strings.Contains(line, "⬡") || strings.Contains(line, "☸") {
		t.Fatalf("a docker row %q is not marked ⬡", line)
	}
}

// A cluster row names its connection, "(missing)" once the daemon has none.
func TestKubeRowTargetMissing(t *testing.T) {
	inst := kubeInst("gone", fleet.StatusStopped, "")
	inst.KubeCluster = "old-1"
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, inst)
	if line := rowLine(t, m, fp, "gone"); !strings.Contains(line, "☸ old-1 (missing)") {
		t.Fatalf("row %q", line)
	}
}

// Failed and parked cluster rows say what their code says; an interrupted
// one offers what can be done today (d).
func TestKubeFailedRowNotes(t *testing.T) {
	cases := []struct {
		inst *fleet.Instance
		want string
	}{
		{kubeInst("a", fleet.StatusFailed, interruptedErr), "failed · interrupted · d delete"},
		{kubeInst("b", fleet.StatusFailed, "kubernetes:rebuild_failed: generation 2 was restored"), "failed · rebuild failed"},
		{kubeInst("c", fleet.StatusFailed, "kubernetes:tampered: pod fi-x-0: image digest sha256:1 is not the one fleet delivered"), "failed · pod tampered with"},
		{kubeInst("d", fleet.StatusFailed, "kubernetes:quota: exceeded quota: compute"), "failed · over quota"},
		{kubeInst("e", fleet.StatusStopped, oomParkedErr), "stopped · parked: out of memory"},
		{kubeInst("f", fleet.StatusStopped, "kubernetes:crash_loop: the dev container kept crashing"), "stopped · parked: crash loop"},
		{kubeInst("g", fleet.StatusFailed, "devcontainer up failed"), "failed"},
		// A stopped record's other codes are not a parked crash loop's.
		{kubeInst("h", fleet.StatusStopped, interruptedErr), "stopped"},
	}
	for _, tc := range cases {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, tc.inst)
		line := rowLine(t, m, fp, tc.inst.Name)
		if !strings.Contains(line, tc.want) {
			t.Errorf("%s: row %q lacks %q", tc.inst.Name, line, tc.want)
		}
		if tc.want == "failed" || tc.want == "stopped" {
			if strings.Contains(line, tc.want+" ·") {
				t.Errorf("%s: row %q has a note", tc.inst.Name, line)
			}
		}
	}
	// Only cluster rows read kubernetes codes.
	docker := &fleet.Instance{Name: "z", Status: fleet.StatusFailed, Backend: fleet.BackendDevcontainer, Error: interruptedErr}
	if note := kubeFailedNote(docker); note != "" {
		t.Errorf("a docker row's note %q", note)
	}
}

// enter on a failed or parked cluster row opens the recovery dialog with the
// buttons its state allows; on a running one it does not.
func TestEnterOpensKubeRecovery(t *testing.T) {
	cases := []struct {
		inst *fleet.Instance
		want []kubeRecoveryAction
	}{
		{kubeInst("a", fleet.StatusFailed, interruptedErr), []kubeRecoveryAction{kubeRecoveryLogs, kubeRecoveryDestroy, kubeRecoveryClose}},
		{kubeInst("b", fleet.StatusStopped, oomParkedErr), []kubeRecoveryAction{kubeRecoverySettings, kubeRecoveryLogs, kubeRecoveryStart, kubeRecoveryDestroy, kubeRecoveryClose}},
		{kubeInst("c", fleet.StatusFailed, "kubernetes:tampered: refused"), []kubeRecoveryAction{kubeRecoveryLogs, kubeRecoveryDestroy, kubeRecoveryClose}},
		{kubeInst("d", fleet.StatusFailed, "kubernetes:quota: exceeded quota"), []kubeRecoveryAction{kubeRecoverySettings, kubeRecoveryLogs, kubeRecoveryDestroy, kubeRecoveryClose}},
	}
	for _, tc := range cases {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, tc.inst)
		fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
		if fp.mode != viewKubeRecovery {
			t.Fatalf("%s: enter left mode %v", tc.inst.Name, fp.mode)
		}
		if got := fp.kubeRecovery.actions(); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: actions %v, want %v", tc.inst.Name, got, tc.want)
		}
		view := flatText(fp.viewFleetList(m))
		if !strings.Contains(view, "Kubernetes: alpha/"+tc.inst.Name) || !strings.Contains(view, "☸ minikube") {
			t.Errorf("%s: dialog %q", tc.inst.Name, view)
		}
	}

	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("a", fleet.StatusFailed, interruptedErr))
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
	view := flatText(fp.viewFleetList(m))
	for _, want := range []string{"was interrupted on ☸ minikube", "interrupted by daemon restart during create (at step seed)",
		kubeerr.Hint(kubeerr.Interrupted), "[ View the creation log ]", "[ Destroy instance ]", "[ Close ]"} {
		if !strings.Contains(view, want) {
			t.Errorf("the interrupted dialog lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Retry") {
		t.Errorf("the dialog offers a retry before rebuild exists:\n%s", view)
	}
}

// enter on a running cluster row whose pod fleet refuses (tampered with)
// opens the dialog rather than a shell no session can reach; its footer says
// so. A running row with a pod that is merely not ready still opens a shell.
func TestEnterOnARefusedPodOpensKubeRecovery(t *testing.T) {
	inst := kubeInst("t", fleet.StatusRunning, "")
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, inst)
	m.runtime["alpha/t"] = &fleetgrpc.InstanceRuntime{Fleet: "alpha", Instance: "t",
		Kube: &fleetgrpc.KubeInstanceRuntime{PodReason: "kubernetes:tampered"}}
	keys := strings.Join(fp.contextualHelpKeys(m), "  ")
	if !strings.Contains(keys, "enter: details") || strings.Contains(keys, "open shell") {
		t.Errorf("a refused pod's footer %q", keys)
	}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
	if fp.mode != viewKubeRecovery {
		t.Fatalf("enter on a refused pod left mode %v", fp.mode)
	}
	want := []kubeRecoveryAction{kubeRecoveryLogs, kubeRecoveryDestroy, kubeRecoveryClose}
	if got := fp.kubeRecovery.actions(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("actions %v, want %v", got, want)
	}
	view := flatText(fp.viewFleetList(m))
	for _, w := range []string{"Kubernetes: alpha/t has a pod fleet did not make on ☸ minikube", kubeerr.Hint(kubeerr.Tampered), "[ View logs ]"} {
		if !strings.Contains(view, w) {
			t.Errorf("the dialog lacks %q:\n%s", w, view)
		}
	}

	if _, ok := kubePodRecoveryCode(inst, &fleetgrpc.InstanceRuntime{Kube: &fleetgrpc.KubeInstanceRuntime{PodReason: "ImagePullBackOff"}}); ok {
		t.Error("a pod that is not ready yet reads as refused")
	}
	stopped := kubeInst("s", fleet.StatusStopped, "")
	if _, ok := kubePodRecoveryCode(stopped, m.runtime["alpha/t"]); ok {
		t.Error("a stopped instance reads as a refused pod")
	}
	docker := &fleet.Instance{Name: "d", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer}
	if _, ok := kubePodRecoveryCode(docker, m.runtime["alpha/t"]); ok {
		t.Error("a docker instance reads as a refused pod")
	}
}

// The recovery dialog's buttons: Destroy goes through the usual
// confirmation, Start starts a parked instance, the settings button opens
// the cluster editor on the field that fixes it, Close closes.
func TestKubeRecoveryActions(t *testing.T) {
	t.Run("destroy", func(t *testing.T) {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("a", fleet.StatusFailed, interruptedErr))
		fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
		fp.kubeRecovery.cursor = 1
		fp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
		if fp.mode != viewConfirmDelete || fp.dlg.fleet != "alpha" || fp.dlg.inst != "a" {
			t.Fatalf("destroy: mode %v dlg %+v", fp.mode, fp.dlg)
		}
	})
	t.Run("start", func(t *testing.T) {
		inst := kubeInst("b", fleet.StatusStopped, oomParkedErr)
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, inst)
		fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
		fp.kubeRecovery.cursor = 2
		if cmd := fp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
			t.Fatal("start ran nothing")
		}
		if fp.mode != viewNormal || inst.Status != fleet.StatusStarting {
			t.Fatalf("start: mode %v status %s", fp.mode, inst.Status)
		}
	})
	t.Run("raise memory", func(t *testing.T) {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("b", fleet.StatusStopped, oomParkedErr))
		fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
		fp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}) // the first button
		sp, ok := m.currentPage.(*settingsPage)
		if !ok {
			t.Fatalf("the settings button left page %T", m.currentPage)
		}
		e, ok := sp.kubeDialog.(*kubeEditor)
		if !ok || e.id != "mk-1" {
			t.Fatalf("no cluster editor for mk-1: %#v", sp.kubeDialog)
		}
		if row := e.current(); row.kind != kubeEditorRowField || row.field.path != "memory_limit" {
			t.Fatalf("the editor opened on %+v, want memory_limit", row)
		}
	})
	t.Run("close", func(t *testing.T) {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("a", fleet.StatusFailed, interruptedErr))
		fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
		fp.Update(m, tea.KeyMsg{Type: tea.KeyEscape})
		if fp.mode != viewNormal {
			t.Fatalf("esc left mode %v", fp.mode)
		}
	})
}

// A create the TUI started that fails with a code, or that the daemon
// refuses with one before any record exists, opens the dialog on its own.
func TestKubeRecoveryOpensForACreate(t *testing.T) {
	inst := kubeInst("a", fleet.StatusFailed, interruptedErr)
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, inst)
	if !fp.openKubeRecovery("alpha", inst) || fp.mode != viewKubeRecovery {
		t.Fatal("no dialog for a failed create")
	}

	fp.mode = viewNormal
	refusal := grpcstatus.Error(codes.ResourceExhausted, `kubernetes:quota: connection "minikube" allows 2 instances and has 2`)
	if !fp.openKubeRecoveryForRefusal("alpha", "mk-1", refusal) {
		t.Fatal("no dialog for a refused create")
	}
	if got := fp.kubeRecovery.actions(); fmt.Sprint(got) != fmt.Sprint([]kubeRecoveryAction{kubeRecoverySettings, kubeRecoveryClose}) {
		t.Errorf("refusal actions %v", got)
	}
	view := flatText(fp.viewFleetList(m))
	if !strings.Contains(view, "a new instance in alpha has no room under the quotas on ☸ minikube") || !strings.Contains(view, "[ Raise the quotas (cluster resources) ]") {
		t.Errorf("refusal dialog:\n%s", view)
	}
	fp.mode = viewNormal
	if fp.openKubeRecoveryForRefusal("alpha", "mk-1", errors.New("connection refused")) || fp.mode != viewNormal {
		t.Error("an uncoded refusal opened the dialog")
	}
}

// Every key the recovery dialog acts on is in its table (the footer and the
// Dialogs group).
func TestKubeRecoveryKeysAreDocumented(t *testing.T) {
	checkKubeKeys(t, "the recovery dialog", [][]kubeKey{kubeRecoveryKeys}, func(t *testing.T) (func(tea.KeyMsg) tea.Cmd, func() string) {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("b", fleet.StatusStopped, oomParkedErr))
		fp.openKubeRecovery("alpha", m.st.Fleets["alpha"].Instances[0])
		return func(k tea.KeyMsg) tea.Cmd { return fp.Update(m, k) }, func() string {
			return fmt.Sprint(fp.mode, fp.kubeRecovery.cursor, m.currentPage == kubePage(fp))
		}
	})
	footer := kubeFooter(kubeRecoveryKeys, nil)
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("b", fleet.StatusStopped, oomParkedErr))
	fp.openKubeRecovery("alpha", m.st.Fleets["alpha"].Instances[0])
	if view := fp.viewFleetList(m); !strings.Contains(flatText(view), flatText(footer)) {
		t.Errorf("the dialog does not show its footer %q", footer)
	}
}

func kubePage(fp *fleetPage) Page { return fp }

// The footer of a failed or parked cluster row names every key that acts on
// it, and the keybindings list names enter's meaning there.
func TestKubeRecoverableRowFooter(t *testing.T) {
	for _, inst := range []*fleet.Instance{kubeInst("a", fleet.StatusFailed, interruptedErr), kubeInst("b", fleet.StatusStopped, oomParkedErr)} {
		m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, inst)
		keys := strings.Join(fp.contextualHelpKeys(m), "  ")
		for _, want := range []string{"enter: details", "d: delete", "L: logs"} {
			if !strings.Contains(keys, want) {
				t.Errorf("%s footer %q lacks %q", inst.Status, keys, want)
			}
		}
		if strings.Contains(keys, "open shell") || strings.Contains(keys, "R: rebuild") {
			t.Errorf("%s footer %q offers what the row cannot do", inst.Status, keys)
		}
	}
	found := false
	for _, g := range keybindingsData() {
		for _, e := range g.Entries {
			if strings.Contains(e.Key, "failed / parked ☸") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the keybindings list does not say what enter does on a failed cluster row")
	}
}

// The New instance dialog lists the cluster targets it cannot create on,
// with the daemon's reasons, and the picked cluster's health.
func TestDeployRowListsUnavailableClusters(t *testing.T) {
	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	m.kubeClusters.Clusters = append(m.kubeClusters.Clusters, kubeSt("eu-1", "prod-eu", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE))
	list := deployTargetList{options: []fleet.DeployTargetOption{
		{Target: fleet.DeployTarget{Backend: fleet.BackendDevcontainer}, Available: true},
		{Target: fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "mk-1"}, Available: true},
		{Target: fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "eu-1"}, Reason: "cluster unreachable: dial tcp: i/o timeout"},
		{Target: fleet.DeployTarget{Backend: fleet.BackendCoder}, Reason: "coder CLI not found where fleetd runs"},
	}}
	fp.mode = viewAddInstance
	fp.dlg.fleet = "alpha"
	fp.addInst.targets = list.available()
	fp.addInst.unavailable = list.unavailableClusters()
	fp.addInst.target = fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "mk-1"}
	view := flatText(fp.renderAddInstanceDialog(m))
	for _, want := range []string{"[ ☸ minikube ] ● ready", "☸ prod-eu ✕ cluster unreachable: dial tcp: i/o timeout"} {
		if !strings.Contains(view, want) {
			t.Errorf("the dialog lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "coder CLI") {
		t.Errorf("the dialog lists an unavailable non-cluster target:\n%s", view)
	}
	if len(fp.addInst.targets) != 2 {
		t.Errorf("the Deploy row cycles %v", fp.addInst.targets)
	}
}

// For a remote daemon the logs view reads the logs through this binary's
// `fleet logs` (the Logs RPC), not the daemon host's argv.
func TestLogsViewOfARemoteDaemon(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "https://gw.example.com")
	argv, err := resolveLogsArgv("alpha", "a")
	if err != nil || len(argv) != 3 || argv[1] != "logs" || argv[2] != "alpha/a" {
		t.Fatalf("argv %v %v", argv, err)
	}
}

// A published port that could not listen shows on the row; one on every
// interface of the daemon host shows only when the daemon is remote (on the
// user's own machine it is their own -p 0.0.0.0, as docker's).
func TestKubePublishedPortNotes(t *testing.T) {
	ports := []*fleetgrpc.KubePublishedPort{
		{Address: "127.0.0.1", HostPort: 3000, ContainerPort: 3000},
		{Address: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Error: "the port is in use on the daemon host"},
		{Address: "0.0.0.0", HostPort: 9000, ContainerPort: 9000},
	}
	rt := &fleetgrpc.InstanceRuntime{Fleet: "alpha", Instance: "eta", Kube: &fleetgrpc.KubeInstanceRuntime{PublishedPorts: ports}}

	m, fp := kubeRowModel(t, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, kubeInst("eta", fleet.StatusRunning, ""))
	m.runtime["alpha/eta"] = rt
	fp.cursor = 0
	line := rowLine(t, m, fp, "eta")
	if !strings.Contains(line, "⚠ port 8080 not published: the port is in use on the daemon host") {
		t.Errorf("a conflict should show: %q", line)
	}
	if strings.Contains(line, "9000") || strings.Contains(line, "3000") {
		t.Errorf("a local daemon's ports should not warn: %q", line)
	}

	t.Setenv(fleetclient.EnvServer, "tcp://fleet.example:7000")
	notes, _ := m.kubeRunningNotes(m.st.Fleets["alpha"].Instances[0], rt)
	joined := strings.Join(notes, " · ")
	if !strings.Contains(joined, "⚠ port 9000 open on the daemon host's network (0.0.0.0)") || strings.Contains(joined, "3000") {
		t.Errorf("a remote daemon's wildcard port should warn: %q", joined)
	}
	if kubePortNote(&fleetgrpc.KubePublishedPort{Address: "::", HostPort: 1}, true) == "" || kubePortNote(&fleetgrpc.KubePublishedPort{Address: "10.0.0.5", HostPort: 1}, true) != "" {
		t.Error("only an unspecified address is every interface")
	}
}
