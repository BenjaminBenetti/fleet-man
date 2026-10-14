package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// dialog_kube_remove.go is the disconnect dialog of a cluster row's
// [ delete ]. An unused connection disconnects on confirm. One that instances
// or agents use lists them and offers what RemoveKubeCluster can do with
// them: destroy the instances first (refused while an agent still targets
// the cluster, since it would make new ones there), or forget the connection
// and leave its objects in the cluster, scaled to 0. Forget is offered
// straight away only when the cluster is unreachable; otherwise its name must
// be typed. A context that has gone missing (renamed) offers Re-point instead.

// kubeRemoveAction is one of the dialog's buttons.
type kubeRemoveAction int

const (
	kubeRemoveDisconnect kubeRemoveAction = iota // unused: just remove it
	kubeRemoveRepoint                            // CONTEXT_MISSING: pick the renamed context
	kubeRemoveDestroy                            // DESTROY_INSTANCES
	kubeRemoveForget                             // FORGET
	kubeRemoveCancel
)

// kubeRemoveDialog is the disconnect dialog.
type kubeRemoveDialog struct {
	id                string
	gen               int    // the watch generation it was opened under
	name              string // as it was when the dialog opened (a push may drop it first)
	instances, agents []kubeUser
	cursor            int // index into actions()

	typing bool // asking for the name before a Forget
	input  textinput.Model

	busy     string
	err      string
	warnings []string
}

type kubeRemovedMsg struct {
	dialog *kubeRemoveDialog
	mode   fleetgrpc.KubeRemoveMode
	reply  *fleetgrpc.RemoveKubeClusterReply
	err    error
}

func newKubeRemoveDialog(m *model, id string) *kubeRemoveDialog {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 128
	d := &kubeRemoveDialog{id: id, name: id, input: input, gen: m.watchGen}
	if st := m.kubeStatus(id); st != nil {
		d.name = st.GetCluster().DisplayNameOrID()
	}
	d.instances, d.agents = kubeUsers(m, id)
	return d
}

// used reports whether anything uses the connection.
func (d *kubeRemoveDialog) used() bool { return len(d.instances)+len(d.agents) > 0 }

// actions are the buttons, in order.
func (d *kubeRemoveDialog) actions(st *fleetgrpc.KubeClusterStatus) []kubeRemoveAction {
	if !d.used() {
		return []kubeRemoveAction{kubeRemoveDisconnect, kubeRemoveCancel}
	}
	var out []kubeRemoveAction
	if st.GetState() == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING {
		out = append(out, kubeRemoveRepoint)
	}
	if len(d.instances) > 0 {
		out = append(out, kubeRemoveDestroy)
	}
	return append(out, kubeRemoveForget, kubeRemoveCancel)
}

// destroyBlocked says why Destroy cannot run: agents would recreate
// instances on the cluster.
func (d *kubeRemoveDialog) destroyBlocked() string {
	if len(d.agents) == 0 {
		return ""
	}
	return fmt.Sprintf("%s still %s it: give %s another target first", countNoun(len(d.agents), "agent"),
		map[bool]string{true: "targets", false: "target"}[len(d.agents) == 1], map[bool]string{true: "it", false: "them"}[len(d.agents) == 1])
}

// forgetNeedsName reports whether Forget asks for the name first: unless the
// cluster is unreachable, forgetting abandons objects fleet could still
// clean up, so it takes the deliberate step.
func (d *kubeRemoveDialog) forgetNeedsName(st *fleetgrpc.KubeClusterStatus) bool {
	return st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE
}

func (d *kubeRemoveDialog) update(m *model, page *settingsPage, msg tea.Msg) (tea.Cmd, bool) {
	st := m.kubeStatus(d.id)
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if st == nil && d.busy == "" {
			page.closeKubeDialog(m)
			m.message = fmt.Sprintf("%s %s is no longer connected", kubeGlyphCluster, d.name)
			return nil, true
		}
		return d.key(m, page, st, msg), true
	}
	return nil, false
}

func (d *kubeRemoveDialog) key(m *model, page *settingsPage, st *fleetgrpc.KubeClusterStatus, key tea.KeyMsg) tea.Cmd {
	if d.busy != "" {
		// The daemon is working on it (destroying instances can take
		// minutes); its answer closes or updates the dialog. esc leaves it
		// to finish there, and the answer then comes as a message.
		if s := key.String(); s == "esc" || s == "q" {
			page.closeKubeDialog(m)
			m.message = fmt.Sprintf("%s %s: still %s on fleetd", kubeGlyphCluster, d.name, strings.TrimSuffix(d.busy, "…"))
		}
		return nil
	}
	if d.typing {
		switch key.String() {
		case "esc":
			d.typing = false
			d.input.Blur()
			return nil
		case "enter":
			if strings.TrimSpace(d.input.Value()) != st.GetCluster().DisplayNameOrID() {
				d.err = "that is not its name"
				return nil
			}
			d.typing = false
			d.input.Blur()
			return d.remove(fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET, "forgetting…")
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(key)
		return cmd
	}
	actions := d.actions(st)
	d.cursor = max(0, min(d.cursor, len(actions)-1))
	switch key.String() {
	case "esc", "q":
		page.closeKubeDialog(m)
	case "j", "down", "l", "right", "tab":
		d.cursor = (d.cursor + 1) % len(actions)
	case "k", "up", "h", "left", "shift+tab":
		d.cursor = (d.cursor - 1 + len(actions)) % len(actions)
	case "enter":
		d.err = ""
		switch actions[d.cursor] {
		case kubeRemoveCancel:
			page.closeKubeDialog(m)
		case kubeRemoveDisconnect:
			return d.remove(fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_REFUSE_IF_USED, "disconnecting…")
		case kubeRemoveRepoint:
			picker := newKubePicker(m, d.id)
			page.kubeDialog = picker
			return picker.load(m, nil)
		case kubeRemoveDestroy:
			if why := d.destroyBlocked(); why != "" {
				d.err = why
				return nil
			}
			return d.remove(fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES, "destroying the instances…")
		case kubeRemoveForget:
			if d.forgetNeedsName(st) {
				d.typing = true
				d.input.SetValue("")
				d.input.Placeholder = st.GetCluster().DisplayNameOrID()
				return d.input.Focus()
			}
			return d.remove(fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET, "forgetting…")
		}
	}
	return nil
}

// remove asks the daemon to disconnect the connection in mode.
func (d *kubeRemoveDialog) remove(mode fleetgrpc.KubeRemoveMode, busy string) tea.Cmd {
	d.busy = busy
	id := d.id
	return func() tea.Msg {
		reply, err := removeKubeClusterRemote(id, mode)
		return kubeRemovedMsg{dialog: d, mode: mode, reply: reply, err: err}
	}
}

// handleKubeRemovedMsg applies a disconnect's answer: in its dialog while
// that is open, else (esc left it while fleetd worked) as a message. An
// armada switch drops it: the dialog was about the daemon it left.
func (m *model) handleKubeRemovedMsg(msg kubeRemovedMsg) tea.Cmd {
	d := msg.dialog
	if d == nil || d.gen != m.watchGen {
		return nil
	}
	if page, ok := m.currentPage.(*settingsPage); ok && page.kubeDialog == kubeDialog(d) {
		return d.onRemoved(m, page, msg)
	}
	d.busy = ""
	switch {
	case msg.err != nil:
		m.message = fmt.Sprintf("Disconnecting %s %s failed: %s", kubeGlyphCluster, d.name, kubeRPCError(msg.err))
	case !msg.reply.GetRemoved():
		m.message = fmt.Sprintf("%s %s is still connected: something uses it now", kubeGlyphCluster, d.name)
	default:
		m.kubeForget(d.id)
		m.message = fmt.Sprintf("Disconnected %s %s", kubeGlyphCluster, d.name)
		if w := msg.reply.GetWarnings(); len(w) > 0 {
			m.message += " · " + strings.Join(w, " · ")
		}
	}
	return nil
}

// onRemoved applies the daemon's answer: gone, or (users appeared since the
// dialog opened) the blockers to choose for.
func (d *kubeRemoveDialog) onRemoved(m *model, page *settingsPage, msg kubeRemovedMsg) tea.Cmd {
	d.busy = ""
	if msg.err != nil {
		d.err = kubeRPCError(msg.err)
		return nil
	}
	if !msg.reply.GetRemoved() {
		d.instances, d.agents = kubeUsersFromReply(msg.reply.GetUsers())
		d.cursor = 0
		d.err = "it is in use now: choose what happens to what uses it"
		return nil
	}
	m.kubeForget(d.id)
	page.closeKubeDialog(m)
	page.cursorToItem(m, settingsItemKubeAdd)
	m.message = fmt.Sprintf("Disconnected %s %s", kubeGlyphCluster, d.name)
	if w := msg.reply.GetWarnings(); len(w) > 0 {
		m.message += " · " + strings.Join(w, " · ")
	}
	return nil
}

func (d *kubeRemoveDialog) view(m *model, _ *settingsPage) string {
	const maxWidth = 72
	inner := kubeDialogInner(m, maxWidth)
	st := m.kubeStatus(d.id)
	if st == nil {
		return kubeDialogFrame(m, dimStyle.Render("This connection is gone.")+"\n"+dialogHint.Render("[esc] close"), maxWidth)
	}
	name := st.GetCluster().DisplayNameOrID()
	lines := []string{dialogTitle.Render("Disconnect " + kubeGlyphCluster + " " + name)}
	if !d.used() {
		lines = append(lines, dimStyle.Render("No instance or agent uses this cluster."),
			dimStyle.Render("Its objects stay in namespace "+st.GetCluster().GetNamespace()+"; fleet's owner Lease is released."))
	} else {
		var what []string
		if len(d.instances) > 0 {
			what = append(what, countNoun(len(d.instances), "instance"))
		}
		if len(d.agents) > 0 {
			what = append(what, countNoun(len(d.agents), "agent"))
		}
		verb := "uses"
		if len(d.instances)+len(d.agents) > 1 {
			verb = "use"
		}
		lines = append(lines, strings.Join(what, " and ")+" still "+verb+" this cluster:")
		for _, u := range append(append([]kubeUser(nil), d.instances...), d.agents...) {
			lines = append(lines, "  • "+u.String())
		}
		if st.GetState() == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING {
			lines = append(lines, dimStyle.Render("Context renamed? Use [ Re-point ] instead."))
		}
	}
	lines = append(lines, "")
	actions := d.actions(st)
	d.cursor = max(0, min(d.cursor, len(actions)-1))
	for i, a := range actions {
		marker := "  "
		if i == d.cursor {
			marker = cursorStyle.Render("> ")
		}
		label, note := d.actionLabel(a, st)
		switch {
		case i == d.cursor:
			label = selectedStyle.Render(label)
		case a == kubeRemoveDestroy && d.destroyBlocked() != "":
			label = dimStyle.Render(label)
		}
		line := marker + label
		if note != "" {
			line += "  " + dimStyle.Render(note)
		}
		lines = append(lines, wrapToWidth(line, inner))
	}
	if d.typing {
		lines = append(lines, "", "Type "+selectedStyle.Render(name)+" to forget it: "+d.input.View())
	}
	if d.busy != "" {
		lines = append(lines, "", m.spinner.View()+" "+dimStyle.Render(d.busy))
	}
	if d.err != "" {
		lines = append(lines, "", wrapToWidth(kubeErrStyle.Render(kubeGlyphFailed+" "+d.err), inner))
	}
	hint := kubeFooter(kubeRemoveKeys, nil)
	if d.typing {
		hint = "[enter] forget  [esc] back"
	}
	lines = append(lines, dialogHint.Render(hint))
	return kubeDialogFrame(m, strings.Join(lines, "\n"), maxWidth)
}

// actionLabel is a button's label and the note beside it.
func (d *kubeRemoveDialog) actionLabel(a kubeRemoveAction, st *fleetgrpc.KubeClusterStatus) (string, string) {
	switch a {
	case kubeRemoveDisconnect:
		return "[ Disconnect ]", ""
	case kubeRemoveRepoint:
		return "[ Re-point ]", "pick the context it is called now"
	case kubeRemoveDestroy:
		return fmt.Sprintf("[ Destroy %s, then disconnect ]", countNoun(len(d.instances), "instance")), d.destroyBlocked()
	case kubeRemoveForget:
		note := ""
		if len(d.agents) > 0 {
			names := make([]string, len(d.agents))
			for i, u := range d.agents {
				names[i] = u.fleet + "/" + u.agent
			}
			note = "leaves agent " + strings.Join(names, ", ") + " without a target"
		}
		if d.forgetNeedsName(st) {
			note = strings.TrimPrefix(note+" · asks for the name", " · ")
		}
		return "[ Forget (scale to 0, leave PVCs) ]", note
	}
	return "[ Cancel ]", ""
}
