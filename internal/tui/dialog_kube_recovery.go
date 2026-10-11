package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	tea "github.com/charmbracelet/bubbletea"
	grpcstatus "google.golang.org/grpc/status"
)

// dialog_kube_recovery.go is the Kubernetes recovery dialog: what a coded
// Kubernetes error (kubeerr) means for one instance and what to do about it.
// It opens on its own when a create this TUI started fails, or is refused
// before any record exists, with a code; and on enter on a failed Kubernetes
// row, or on a stopped one the cluster parked (a crash loop, out of memory).
// Its buttons destroy the instance (through the usual confirmation), page
// its logs, start a parked instance again, or open the cluster editor on the
// setting that fixes the error. An interrupted create or rebuild is not
// retried from here yet: destroying it and creating it again is the way.

// kubeRecoveryAction is one of the dialog's buttons.
type kubeRecoveryAction int

const (
	kubeRecoverySettings kubeRecoveryAction = iota
	kubeRecoveryLogs
	kubeRecoveryStart
	kubeRecoveryDestroy
	kubeRecoveryClose
)

// kubeRecoveryState is the open dialog. inst is "" for a create refused
// before its record existed (nothing to destroy or page).
type kubeRecoveryState struct {
	fleet, inst string
	cluster     string
	status      fleet.InstanceStatus
	code        kubeerr.Code
	detail      string
	cursor      int
}

// kubeRecoveryKeys act on the recovery dialog.
var kubeRecoveryKeys = []kubeKey{
	{keys: []string{"j", "k", "up", "down", "left", "right", "h", "l", "tab", "shift+tab"}, label: "j / k", hint: "[j/k] choose", shared: true},
	{keys: []string{"enter"}, label: "enter", hint: "[enter] go", shared: true},
	{keys: []string{"esc", "q"}, label: "q / esc", hint: "[esc] close", shared: true},
}

// kubeFixes is the cluster editor field that fixes each class, and the
// button that opens the editor on it. A class missing here has no setting to
// change (an interrupted job, a tampered pod, a build error).
var kubeFixes = map[kubeerr.Code]struct{ field, label string }{
	kubeerr.OOMKilled:       {"memory_limit", "Raise memory (cluster resources)"},
	kubeerr.Quota:           {"max_instances", "Raise the quotas (cluster resources)"},
	kubeerr.Unschedulable:   {"resources.cpu_request", "Lower the requests (cluster resources)"},
	kubeerr.AdmissionDenied: {"allow_privileged", "Review security (cluster settings)"},
	kubeerr.PVCPending:      {"storage_class", "Pick a storage class (cluster storage)"},
	kubeerr.ImagePull:       {"image.mode", "Change image delivery (cluster image)"},
	kubeerr.ImagePush:       {"image.mode", "Change image delivery (cluster image)"},
	kubeerr.Unauthorized:    {"", "Open the cluster connection"},
	kubeerr.Unreachable:     {"", "Open the cluster connection"},
	kubeerr.Forbidden:       {"", "Open the cluster connection"},
	kubeerr.Misconfigured:   {"", "Open the cluster connection"},
	kubeerr.OwnerConflict:   {"", "Open the cluster connection"},
}

// openKubeRecovery opens the dialog for inst's coded error. ok is false
// (nothing opened) when inst has none the dialog explains.
func (fleetPage *fleetPage) openKubeRecovery(fleetName string, inst *fleet.Instance) bool {
	code, detail, ok := kubeRecoveryCode(inst)
	if !ok {
		return false
	}
	fleetPage.kubeRecovery = kubeRecoveryState{
		fleet: fleetName, inst: inst.Name, cluster: inst.KubeCluster,
		status: inst.Status, code: code, detail: detail,
	}
	fleetPage.mode = viewKubeRecovery
	return true
}

// openKubeRecoveryForRefusal opens the dialog for a create the daemon
// refused with a coded error before any record existed (a quota the
// connection's caps leave no room under). ok is false for any other error.
func (fleetPage *fleetPage) openKubeRecoveryForRefusal(fleetName, cluster string, err error) bool {
	code, detail, ok := kubeerr.Parse(grpcstatus.Convert(err).Message())
	if !ok {
		return false
	}
	fleetPage.kubeRecovery = kubeRecoveryState{fleet: fleetName, cluster: cluster, code: code, detail: detail}
	fleetPage.mode = viewKubeRecovery
	return true
}

// actions are the dialog's buttons, in order.
func (d *kubeRecoveryState) actions() []kubeRecoveryAction {
	var out []kubeRecoveryAction
	if fix, ok := kubeFixes[d.code.Class()]; ok && fix.label != "" && d.cluster != "" {
		out = append(out, kubeRecoverySettings)
	}
	if d.inst != "" {
		out = append(out, kubeRecoveryLogs)
		if d.status == fleet.StatusStopped {
			out = append(out, kubeRecoveryStart)
		}
		out = append(out, kubeRecoveryDestroy)
	}
	return append(out, kubeRecoveryClose)
}

// actionLabel is a button's label.
func (d *kubeRecoveryState) actionLabel(a kubeRecoveryAction) string {
	switch a {
	case kubeRecoverySettings:
		return "[ " + kubeFixes[d.code.Class()].label + " ]"
	case kubeRecoveryLogs:
		if d.status == fleet.StatusFailed {
			return "[ View the creation log ]"
		}
		return "[ View logs ]"
	case kubeRecoveryStart:
		return "[ Start anyway ]"
	case kubeRecoveryDestroy:
		return "[ Destroy instance ]"
	}
	return "[ Close ]"
}

// title says what happened, to which instance, on which cluster.
func (d *kubeRecoveryState) title(m *model) string {
	where := ""
	if d.cluster != "" {
		where = " on " + deployTargetLabel(m, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: d.cluster})
	}
	who := d.fleet + "/" + d.inst
	if d.inst == "" {
		who = "a new instance in " + d.fleet
	}
	var what string
	switch d.code.Class() {
	case kubeerr.OOMKilled:
		what = "kept running out of memory"
	case kubeerr.CrashLoop:
		what = "kept crashing"
	case kubeerr.Interrupted:
		what = "was interrupted"
	case kubeerr.RebuildFailed:
		what = "failed to rebuild"
	case kubeerr.Tampered:
		what = "has a pod fleet did not make"
	case kubeerr.Quota:
		what = "is over quota"
		if d.inst == "" {
			what = "has no room under the quotas"
		}
	default:
		what = "failed: " + kubeClassWords[d.code.Class()]
	}
	return "Kubernetes: " + who + " " + what + where
}

func (fleetPage *fleetPage) updateKubeRecovery(m *model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	d := &fleetPage.kubeRecovery
	actions := d.actions()
	switch key.String() {
	case "j", "down", "l", "right", "tab":
		d.cursor = (d.cursor + 1) % len(actions)
	case "k", "up", "h", "left", "shift+tab":
		d.cursor = (d.cursor - 1 + len(actions)) % len(actions)
	case "esc", "q":
		fleetPage.mode = viewNormal
	case "enter":
		d.cursor = max(0, min(d.cursor, len(actions)-1))
		return fleetPage.runKubeRecovery(m, actions[d.cursor])
	}
	return nil
}

// runKubeRecovery does what button a says.
func (fleetPage *fleetPage) runKubeRecovery(m *model, a kubeRecoveryAction) tea.Cmd {
	d := fleetPage.kubeRecovery
	fleetPage.mode = viewNormal
	switch a {
	case kubeRecoverySettings:
		return m.openKubeEditorAt(d.cluster, kubeFixes[d.code.Class()].field)
	case kubeRecoveryLogs:
		inst := fleetPage.recoveryInstance(m)
		if inst == nil {
			m.message = fmt.Sprintf("Instance %s/%s is gone", d.fleet, d.inst)
			return nil
		}
		return execProcess(logsCommand(d.fleet, inst), func(err error) tea.Msg { return execDoneMsg{err} })
	case kubeRecoveryStart:
		inst := fleetPage.recoveryInstance(m)
		if inst == nil || inst.Status != fleet.StatusStopped {
			m.message = fmt.Sprintf("Instance %s/%s is not stopped any more", d.fleet, d.inst)
			return nil
		}
		inst.Status = fleet.StatusStarting // optimistic, like s
		fleetPage.buildRows(m)
		return startInstanceCmd(d.fleet, d.inst)
	case kubeRecoveryDestroy:
		fleetPage.dlg.fleet, fleetPage.dlg.inst = d.fleet, d.inst
		fleetPage.mode = viewConfirmDelete
	}
	return nil
}

// recoveryInstance is the dialog's instance as the TUI has it now, or nil.
func (fleetPage *fleetPage) recoveryInstance(m *model) *fleet.Instance {
	d := fleetPage.kubeRecovery
	if m.st == nil {
		return nil
	}
	f, ok := m.st.Fleets[d.fleet]
	if !ok {
		return nil
	}
	inst, err := f.GetInstance(d.inst)
	if err != nil {
		return nil
	}
	return inst
}

func (fleetPage *fleetPage) renderKubeRecoveryDialog(m *model) string {
	const maxWidth = 76
	d := &fleetPage.kubeRecovery
	inner := kubeDialogInner(m, maxWidth)
	lines := []string{dialogTitle.Render(wrapToWidth(d.title(m), inner))}
	if d.detail != "" {
		lines = append(lines, wrapToWidth(d.detail, inner))
	}
	if hint := kubeerr.Hint(d.code); hint != "" {
		lines = append(lines, wrapToWidth(dimStyle.Render(hint), inner))
	}
	lines = append(lines, dimStyle.Render(string(d.code)), "")
	actions := d.actions()
	d.cursor = max(0, min(d.cursor, len(actions)-1))
	for i, a := range actions {
		marker, label := "  ", d.actionLabel(a)
		if i == d.cursor {
			marker, label = cursorStyle.Render("> "), selectedStyle.Render(label)
		}
		lines = append(lines, marker+label)
	}
	lines = append(lines, "", dialogHint.Render(kubeFooter(kubeRecoveryKeys, nil)))
	return "\n" + kubeDialogFrame(m, strings.Join(lines, "\n"), maxWidth) + "\n"
}

// openKubeEditorAt opens Settings → Kubernetes on cluster id's editor with
// the group holding field open and the cursor on it ("" leaves the cursor at
// the top). A connection the daemon no longer has opens Settings alone.
func (m *model) openKubeEditorAt(id, field string) tea.Cmd {
	cmd := m.ChangeRoute(routeSettings)
	page, ok := m.currentPage.(*settingsPage)
	if !ok || m.kubeStatus(id) == nil {
		m.message = "That cluster connection is gone; Settings → Kubernetes lists the others"
		return cmd
	}
	page.cursorToKubeCluster(m, id)
	editor := newKubeEditor(id)
	editor.gen = m.watchGen
	if field != "" {
		for _, g := range kubeGroups {
			for _, f := range g.fields {
				if f.path == field {
					editor.open[g.key] = true
				}
			}
		}
		for i, row := range editor.rows() {
			if row.kind == kubeEditorRowField && row.field.path == field {
				editor.row = i
				break
			}
		}
	}
	page.kubeDialog = editor
	return cmd
}
