package tui

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	tea "github.com/charmbracelet/bubbletea"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// deploy_targets.go is the TUI side of ListDeployTargets: the New instance
// dialog's Deploy row and the fleetd rows of Settings -> Tool Status. The
// daemon runs the backend CLIs, so it judges what is available; this machine's
// PATH only matters for its own rows (and for a daemon too old to answer).

// checkTools is the deps.CheckTools seam for this machine's tools; tests stub
// it so nothing depends on which CLIs the host has installed.
var checkTools = deps.CheckTools

// deployTargetList is a ListDeployTargets answer in TUI terms.
type deployTargetList struct {
	options []fleet.DeployTargetOption
	// def is what the daemon resolves an unspecified create in this fleet to,
	// i.e. the remembered default. The dialog preselects it when available.
	def fleet.DeployTarget
}

// available returns the targets a create can go to now, in dialog order. A
// target this build can't name (a newer daemon's kind) reads back as the zero
// target; it is left out rather than offered, and created on, as a second
// "Devcontainer".
func (l deployTargetList) available() []fleet.DeployTarget {
	var out []fleet.DeployTarget
	for _, option := range l.options {
		if option.Available && !option.Target.IsZero() {
			out = append(out, option.Target)
		}
	}
	return out
}

// preferred returns the target the dialog opens on: the default when it is
// available, else the first available target. ok is false when none is.
func (l deployTargetList) preferred() (fleet.DeployTarget, bool) {
	available := l.available()
	if len(available) == 0 {
		return fleet.DeployTarget{}, false
	}
	for _, target := range available {
		if target == l.def {
			return target, true
		}
	}
	return available[0], true
}

// loadDeployTargets asks the daemon where a create in fleetName can go. It runs
// on the Update loop like the other synchronous RPCs here (bounded by
// mutationTimeout). A daemon that predates ListDeployTargets is answered the
// way this TUI used to: by this machine's PATH and its config's default.
func loadDeployTargets(m *model, fleetName string, template bool) (deployTargetList, error) {
	reply, err := listDeployTargetsRemote(fleetName)
	if grpcstatus.Code(err) == grpccodes.Unimplemented {
		return legacyDeployTargets(m, template), nil
	}
	if err != nil {
		return deployTargetList{}, err
	}
	return deployTargetList{
		options: protoconv.DeployTargetOptionsFromProto(reply.GetTargets()),
		def:     protoconv.DeployTargetFromProto(reply.GetDefaultTarget()),
	}, nil
}

// legacyDeployTargets judges the targets by this machine's PATH, for a daemon
// without ListDeployTargets (on a local daemon it is the same host).
func legacyDeployTargets(m *model, template bool) deployTargetList {
	m.toolStatus = checkTools()
	found := map[string]bool{}
	for _, tool := range m.toolStatus {
		found[tool.Binary] = tool.Found
	}
	var list deployTargetList
	for _, target := range fleet.DeployTargets() {
		if template && !target.CopiesTemplates() {
			continue
		}
		option := fleet.DeployTargetOption{Target: target, Available: true}
		if tool := target.Tool(); tool != "" && !found[tool] {
			option.Available = false
			option.Reason = fmt.Sprintf("%s CLI not found", tool)
		}
		list.options = append(list.options, option)
	}
	if m.config != nil {
		list.def = fleet.DeployTarget{Backend: fleet.BackendType(m.config.DefaultBackend)}
	}
	return list
}

// noDeployTargetsMessage explains an empty Deploy row.
func noDeployTargetsMessage() string {
	if fleetclient.IsRemote() {
		return "No deploy targets available – install devcontainer or coder CLI where fleetd runs"
	}
	return "No deploy targets available – install devcontainer or coder CLI"
}

// boundTargetLabel is the deploy target an existing instance is bound to,
// as the Edit instance dialog shows it: a cluster the daemon no longer
// connects to (removed with Forget) is named so.
func boundTargetLabel(m *model, target fleet.DeployTarget) string {
	label := deployTargetLabel(m, target)
	if target.KubeCluster != "" && m.kubeClusters != nil && m.kubeStatus(target.KubeCluster) == nil {
		label += " (missing)"
	}
	return label
}

// deployTargetLabel is a target's human-readable name: a cluster target is
// "☸ <its connection's name>".
func deployTargetLabel(m *model, target fleet.DeployTarget) string {
	if target.KubeCluster == "" {
		return backendTypeLabel(target.Backend)
	}
	if st := m.kubeStatus(target.KubeCluster); st != nil {
		return kubeGlyphCluster + " " + st.GetCluster().DisplayNameOrID()
	}
	return kubeGlyphCluster + " " + target.KubeCluster
}

// ===========================================
// fleetd rows of Settings -> Tool Status
// ===========================================

// daemonToolStatus is the daemon host's build tools (ListDeployTargets
// host_tools), shown below this machine's in Settings -> Tool Status.
type daemonToolStatus struct {
	host  string
	tools []deps.ToolStatus
	// err says why the daemon's tools aren't shown; set only when tools is empty.
	err string
}

// daemonToolsMsg carries a daemonToolStatus fetched for watch generation gen,
// so an answer from the daemon a switch left is dropped.
type daemonToolsMsg struct {
	gen    int
	status daemonToolStatus
}

// loadDaemonToolsCmd fetches the daemon host's tools off the Update loop.
func loadDaemonToolsCmd(gen int) tea.Cmd {
	return func() tea.Msg {
		return daemonToolsMsg{gen: gen, status: fetchDaemonTools()}
	}
}

func fetchDaemonTools() daemonToolStatus {
	reply, err := listDeployTargetsRemote("")
	if grpcstatus.Code(err) == grpccodes.Unimplemented {
		return daemonToolStatus{err: "this fleetd is too old to report its tools"}
	}
	if err != nil {
		return daemonToolStatus{err: "could not ask fleetd: " + grpcstatus.Convert(err).Message()}
	}
	return daemonToolStatus{host: reply.GetHost(), tools: protoconv.HostToolsFromProto(reply.GetHostTools())}
}

// handleDaemonToolsMsg stores the daemon's tools. The rows below them shift
// when they arrive, so an open Settings page keeps its cursor on the same row.
func (m *model) handleDaemonToolsMsg(msg daemonToolsMsg) {
	if msg.gen != m.watchGen {
		return
	}
	settingsPage, _ := m.currentPage.(*settingsPage)
	item := -1
	if settingsPage != nil {
		item = settingsPage.settingsCursorItem(m)
	}
	status := msg.status
	m.daemonTools = &status
	if settingsPage != nil && item >= 0 {
		settingsPage.cursorToItem(m, item)
	}
}
