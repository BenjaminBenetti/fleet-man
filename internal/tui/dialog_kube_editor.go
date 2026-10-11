package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dialog_kube_editor.go is the cluster editor: every setting of one
// connection, grouped as the plan's mockup draws them, with the last
// preflight's findings beside them. Each change saves at once through
// UpdateKubeCluster with a FieldMask naming only that field and the revision
// the editor read, so it never writes over a newer change made elsewhere.
// Every field, Security and Advanced included, is editable from a remote TUI
// exactly as from a local one.

// kubeEditorRowKind is what an editor row is.
type kubeEditorRowKind int

const (
	kubeEditorRowField kubeEditorRowKind = iota
	kubeEditorRowGroup
	kubeEditorRowStatus
)

// kubeEditorRow is one navigable row.
type kubeEditorRow struct {
	kind  kubeEditorRowKind
	field *kubeField
	group *kubeGroup
}

// kubeEditor is the cluster editor dialog.
type kubeEditor struct {
	id     string
	gen    int // the watch generation it was opened under
	row    int
	offset int
	open   map[string]bool
	// button is the row's sub-cursor: 0 on the row, 1.. on its buttons (the
	// Status row's [ test ], [ deep test ] and [ login on fleetd ], the
	// Namespace row's [ Create ]).
	button int

	editing *kubeField
	input   textinput.Model
	editRev int64

	// saving marks the fields with a save queued or in flight. Saves go to
	// the daemon one at a time, in order: each one's expected revision is
	// moved past this editor's own earlier saves (own: the revision a save
	// was sent at → the one it produced), so quick edits of two fields never
	// refuse each other as "changed elsewhere", while a change made anywhere
	// else still does.
	saving   map[string]bool
	inFlight bool
	queue    []kubeSave
	own      map[int64]int64
	fieldErr map[string]string
	ns       *kubeNamespacePanel
}

// kubeSave is one field's save waiting for the one in flight.
type kubeSave struct {
	field kubeField
	src   *fleetgrpc.KubeCluster
	rev   int64
}

// Answers to the editor's saves and its $EDITOR run.
type (
	kubeSavedMsg struct {
		editor *kubeEditor
		field  kubeField
		rev    int64 // the revision it was sent at
		status *fleetgrpc.KubeClusterStatus
		err    error
	}
	kubeYAMLEditedMsg struct {
		editor *kubeEditor
		field  kubeField
		before string
		text   string
		rev    int64
		err    error
	}
)

func newKubeEditor(id string) *kubeEditor {
	open := map[string]bool{}
	for _, g := range kubeGroups {
		open[g.key] = g.open
	}
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 4096
	return &kubeEditor{id: id, open: open, input: input, saving: map[string]bool{}, own: map[int64]int64{}, fieldErr: map[string]string{}}
}

// rows is the editor's navigable rows in order.
func (e *kubeEditor) rows() []kubeEditorRow {
	var out []kubeEditorRow
	for i := range kubeTopFields {
		out = append(out, kubeEditorRow{kind: kubeEditorRowField, field: &kubeTopFields[i]})
	}
	out = append(out, kubeEditorRow{kind: kubeEditorRowStatus})
	for gi := range kubeGroups {
		g := &kubeGroups[gi]
		out = append(out, kubeEditorRow{kind: kubeEditorRowGroup, group: g})
		if !e.open[g.key] {
			continue
		}
		for fi := range g.fields {
			out = append(out, kubeEditorRow{kind: kubeEditorRowField, field: &g.fields[fi], group: g})
		}
	}
	return out
}

// current is the row under the cursor.
func (e *kubeEditor) current() kubeEditorRow {
	rows := e.rows()
	e.row = max(0, min(e.row, len(rows)-1))
	return rows[e.row]
}

// buttons is how many buttons the current row has.
func (e *kubeEditor) buttons(m *model, st *fleetgrpc.KubeClusterStatus) int {
	row := e.current()
	switch {
	case row.kind == kubeEditorRowStatus:
		return kubeEditorButtonLogin
	case row.kind == kubeEditorRowField && row.field.path == "namespace" && kubeNamespaceMissing(st.GetChecks()):
		return 1
	}
	return 0
}

func (e *kubeEditor) update(m *model, page *settingsPage, msg tea.Msg) (tea.Cmd, bool) {
	st := m.kubeStatus(e.id)
	switch msg := msg.(type) {
	case kubeYAMLEditedMsg:
		if msg.editor != e {
			return nil, false
		}
		if msg.err != nil {
			e.fieldErr[msg.field.path] = "editor: " + msg.err.Error()
			return nil, true
		}
		if strings.TrimSpace(msg.text) == strings.TrimSpace(msg.before) {
			m.message = msg.field.label + " unchanged"
			return nil, true
		}
		return e.save(msg.field, msg.text, msg.rev), true
	case kubeNamespaceMsg:
		if e.ns == nil || msg.panel != e.ns {
			return nil, false
		}
		if e.ns.handle(msg) {
			e.ns = nil
			m.message = "Namespace " + msg.panel.name + " created; testing again"
			return m.startKubeTest(kubeTestKey(e.id), e.id, &fleetgrpc.TestKubeClusterRequest{Id: e.id}), true
		}
		return nil, true
	case kubeLoginHereDoneMsg:
		if msg.owner != kubeDialog(e) {
			return nil, false
		}
		if msg.err != nil {
			m.message = "Login plugin: " + msg.err.Error()
			return nil, true
		}
		m.message = "Login plugin finished; testing again"
		return m.startKubeTest(kubeTestKey(e.id), e.id, &fleetgrpc.TestKubeClusterRequest{Id: e.id}), true
	case tea.KeyMsg:
		if st == nil {
			page.closeKubeDialog(m)
			m.message = "That connection is gone"
			return nil, true
		}
		return e.key(m, page, st, msg), true
	}
	return nil, false
}

func (e *kubeEditor) key(m *model, page *settingsPage, st *fleetgrpc.KubeClusterStatus, key tea.KeyMsg) tea.Cmd {
	if e.ns != nil {
		cmd, closed := e.ns.key(m, key)
		if closed {
			e.ns = nil
		}
		return cmd
	}
	if e.editing != nil {
		switch key.String() {
		case "esc":
			e.editing = nil
			e.input.Blur()
			return nil
		case "enter":
			f := *e.editing
			e.editing = nil
			e.input.Blur()
			if strings.TrimSpace(e.input.Value()) == strings.TrimSpace(f.text(st.GetCluster())) {
				return nil
			}
			return e.save(f, e.input.Value(), e.editRev)
		}
		var cmd tea.Cmd
		e.input, cmd = e.input.Update(key)
		return cmd
	}

	row := e.current()
	switch key.String() {
	case "esc", "q":
		page.closeKubeDialog(m)
		return nil
	case "j", "down":
		e.row = (e.row + 1) % len(e.rows())
		e.button = 0
	case "k", "up":
		n := len(e.rows())
		e.row = (e.row - 1 + n) % n
		e.button = 0
	case "left", "h", "right", "l":
		dir := 1
		if key.String() == "left" || key.String() == "h" {
			dir = -1
		}
		switch {
		case row.kind == kubeEditorRowGroup:
			e.open[row.group.key] = dir > 0
		case e.buttons(m, st) > 0:
			e.button = max(0, min(e.buttons(m, st), e.button+dir))
		case row.kind == kubeEditorRowField && row.field.cycles():
			return e.cycle(st, *row.field, dir)
		}
	case "enter", " ":
		return e.enter(m, page, st, row)
	case "c":
		if kubeNamespaceMissing(st.GetChecks()) {
			return e.openNamespace(st)
		}
	case "L":
		if kubeLoginHereAvailable() && kubeStatusUsesPlugin(st) {
			return kubeLoginHereCmd(e, st.GetCluster())
		}
	}
	return nil
}

// enter acts on the current row.
func (e *kubeEditor) enter(m *model, page *settingsPage, st *fleetgrpc.KubeClusterStatus, row kubeEditorRow) tea.Cmd {
	switch row.kind {
	case kubeEditorRowGroup:
		e.open[row.group.key] = !e.open[row.group.key]
		return nil
	case kubeEditorRowStatus:
		switch e.button {
		case kubeEditorButtonLogin:
			m.message = fmt.Sprintf("Running the login plugin of %s %s on fleetd…", kubeGlyphCluster, st.GetCluster().DisplayNameOrID())
			return m.startKubeLogin(e.id)
		case kubeEditorButtonDeep:
			m.message = "Deep test: throwaway pods in " + st.GetCluster().GetNamespace() + " (minutes)"
			return m.startKubeTest(kubeTestKey(e.id), e.id, &fleetgrpc.TestKubeClusterRequest{Id: e.id, Deep: true})
		}
		return m.startKubeTest(kubeTestKey(e.id), e.id, &fleetgrpc.TestKubeClusterRequest{Id: e.id})
	}
	f := *row.field
	if e.saving[f.path] {
		return nil
	}
	switch {
	case f.path == "namespace" && e.button == 1:
		return e.openNamespace(st)
	case f.path == kubeDeliveryPath:
		return e.cycleDelivery(st, f, 1, true)
	case f.kind == kubeKindContext:
		picker := newKubePicker(m, e.id)
		picker.back = e
		page.kubeDialog = picker
		return picker.load(m, nil)
	case f.kind == kubeKindYAML:
		return kubeEditYAMLCmd(e, f, f.text(st.GetCluster()), st.GetCluster().GetRevision())
	case f.kind == kubeKindChoice, f.kind == kubeKindBool, f.kind == kubeKindTri:
		return e.cycle(st, f, 1)
	}
	e.editing = &f
	e.editRev = st.GetCluster().GetRevision()
	e.input.SetValue(f.text(st.GetCluster()))
	e.input.Placeholder = kubeInputHint(f)
	e.input.CursorEnd()
	delete(e.fieldErr, f.path)
	return e.input.Focus()
}

// cycle saves the next value of a choice-like field.
func (e *kubeEditor) cycle(st *fleetgrpc.KubeClusterStatus, f kubeField, dir int) tea.Cmd {
	if f.path == kubeDeliveryPath {
		return e.cycleDelivery(st, f, dir, false)
	}
	if e.saving[f.path] {
		return nil // one save at a time per field: the next press reads its result
	}
	next := f.next(st, dir)
	if next == f.text(st.GetCluster()) {
		return nil // nothing else to pick (a class list fleetd could not read)
	}
	return e.save(f, next, st.GetCluster().GetRevision())
}

// cycleDelivery moves the Delivery selector: ←/→ step over a greyed-out
// Gateway and stop at either end; enter (wrap) goes on round to Auto, which
// is never greyed out.
func (e *kubeEditor) cycleDelivery(st *fleetgrpc.KubeClusterStatus, f kubeField, dir int, wrap bool) tea.Cmd {
	if e.saving[f.path] {
		return nil
	}
	next, ok := kubeDeliveryNext(st, dir)
	if !ok {
		if !wrap {
			return nil
		}
		next = kubeDeliveryAuto
	}
	if next == kubeDeliverySetting(st) {
		return nil
	}
	return e.save(f, next, st.GetCluster().GetRevision())
}

// openNamespace opens the Create panel for the connection's namespace.
func (e *kubeEditor) openNamespace(st *fleetgrpc.KubeClusterStatus) tea.Cmd {
	var cmd tea.Cmd
	e.ns, cmd = newKubeNamespacePanel(e.id, nil, st.GetCluster().GetNamespace())
	return cmd
}

// save sends one field's new value, refusing to overwrite a record that
// moved past rev.
func (e *kubeEditor) save(f kubeField, text string, rev int64) tea.Cmd {
	src, err := f.source(text)
	if err != nil {
		e.fieldErr[f.path] = err.Error()
		return nil
	}
	delete(e.fieldErr, f.path)
	e.saving[f.path] = true
	if e.inFlight {
		e.queue = append(e.queue, kubeSave{field: f, src: src, rev: rev})
		return nil
	}
	return e.send(kubeSave{field: f, src: src, rev: rev})
}

// send puts one save on its way, past the editor's own earlier saves.
func (e *kubeEditor) send(s kubeSave) tea.Cmd {
	for next, ok := e.own[s.rev]; ok; next, ok = e.own[s.rev] {
		s.rev = next
	}
	e.inFlight = true
	id := e.id
	return func() tea.Msg {
		st, err := updateKubeClusterRemote(id, s.src, []string{s.field.path}, s.rev)
		return kubeSavedMsg{editor: e, field: s.field, rev: s.rev, status: st, err: err}
	}
}

// handleKubeSavedMsg hands a save's answer to the editor that sent it, open
// or not: one closed (or left with Settings) while saves were on their way
// still sends the rest of its queue, so no edit made before esc is lost. An
// armada switch drops them: they were meant for the daemon it left.
func (m *model) handleKubeSavedMsg(msg kubeSavedMsg) tea.Cmd {
	if msg.editor == nil || msg.editor.gen != m.watchGen {
		return nil
	}
	return msg.editor.onSaved(m, msg)
}

// onSaved applies a save's answer, then sends the next queued one.
func (e *kubeEditor) onSaved(m *model, msg kubeSavedMsg) tea.Cmd {
	delete(e.saving, msg.field.path)
	e.inFlight = false
	switch {
	case status.Code(msg.err) == codes.Aborted:
		e.fieldErr[msg.field.path] = "changed elsewhere meanwhile: showing the current value, try again"
	case msg.err != nil:
		e.fieldErr[msg.field.path] = kubeRPCError(msg.err)
	default:
		if rev := msg.status.GetCluster().GetRevision(); rev > msg.rev && msg.rev != 0 {
			e.own[msg.rev] = rev
		}
		m.kubeUpsert(msg.status)
		m.message = "Saved " + msg.field.label
		if msg.field.path == "allow_privileged" && msg.field.text(msg.status.GetCluster()) == "on" {
			m.message = "Privileged pods allowed: " + kubePrivilegedWarning
		}
	}
	if len(e.queue) == 0 {
		return nil
	}
	next := e.queue[0]
	e.queue = e.queue[1:]
	return e.send(next)
}

// kubeInputHint is the input's placeholder for a field: its format.
func kubeInputHint(f kubeField) string {
	switch f.kind {
	case kubeKindList:
		return "comma-separated"
	case kubeKindPaths:
		return "absolute paths on fleetd's host, '" + kubePathSeparator + "'-separated"
	case kubeKindMap:
		return "key=value, key=value"
	case kubeKindTolerations:
		return "key=value:Effect@seconds, key:Effect, * (every taint)"
	case kubeKindInt, kubeKindOptInt:
		return "a whole number; empty = " + f.def
	case kubeKindClass:
		return "a class name; empty = auto"
	}
	if f.def != "" {
		return "empty = " + f.def
	}
	return ""
}

// kubeExternalEditor is the $EDITOR run on a file (a seam for tests).
var kubeExternalEditor = func(path string) *exec.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// $EDITOR may carry arguments ("code --wait"): let the shell split it.
	return exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
}

// kubeEditYAMLCmd edits a YAML field in $EDITOR, then saves what it holds.
func kubeEditYAMLCmd(e *kubeEditor, f kubeField, before string, rev int64) tea.Cmd {
	dir, err := os.MkdirTemp("", "fleet-kube-")
	if err != nil {
		return func() tea.Msg { return kubeYAMLEditedMsg{editor: e, field: f, err: err} }
	}
	path := filepath.Join(dir, strings.ReplaceAll(f.path, "_", "-")+".yaml")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return func() tea.Msg { return kubeYAMLEditedMsg{editor: e, field: f, err: err} }
	}
	return execProcess(kubeExternalEditor(path), func(runErr error) tea.Msg {
		data, readErr := os.ReadFile(path)
		_ = os.RemoveAll(dir)
		return kubeYAMLEditedMsg{editor: e, field: f, before: before, text: string(data), rev: rev, err: errors.Join(runErr, readErr)}
	})
}

// ===========================================
// View
// ===========================================

func (e *kubeEditor) view(m *model, _ *settingsPage) string {
	const maxWidth = 100
	inner := kubeDialogInner(m, maxWidth)
	st := m.kubeStatus(e.id)
	if st == nil {
		return kubeDialogFrame(m, dialogTitle.Render("Edit cluster")+"\n\n"+dimStyle.Render("This connection is gone.")+"\n"+dialogHint.Render("[esc] close"), maxWidth)
	}
	c := st.GetCluster()
	head := []string{dialogTitle.Render("Edit cluster " + kubeGlyphCluster + " " + c.DisplayNameOrID())}

	var body []string
	cursorStart, cursorEnd := 0, 1
	rows := e.rows()
	e.row = max(0, min(e.row, len(rows)-1))
	for i, row := range rows {
		start := len(body)
		marker := "  "
		if i == e.row {
			marker = cursorStyle.Render("> ")
		}
		switch row.kind {
		case kubeEditorRowStatus:
			line := marker + e.labelCell("Status:", i == e.row) + e.statusValue(m, st)
			buttons := kubeButton("[ test ]", i == e.row && e.button == kubeEditorButtonTest) + " " +
				kubeButton("[ deep test ]", i == e.row && e.button == kubeEditorButtonDeep) + " " +
				kubeButton("[ login on fleetd ]", i == e.row && e.button == kubeEditorButtonLogin)
			if lipgloss.Width(line)+2+lipgloss.Width(buttons) <= inner {
				body = kubeLines(body, line+"  "+buttons)
			} else {
				// Too narrow for one line: the buttons go under the health,
				// never split between lines.
				body = kubeLines(body, kubeHangingIndent(line, inner, kubeEditorValueCol), strings.Repeat(" ", kubeEditorValueCol)+buttons)
			}
			body = kubeLines(body, e.statusLines(m, st, inner)...)
		case kubeEditorRowGroup:
			arrow := "▶ "
			if e.open[row.group.key] {
				arrow = "▼ "
			}
			title := arrow + row.group.title
			if i == e.row {
				title = selectedStyle.Render(title)
			} else {
				title = dialogLabel.Render(title)
			}
			line := marker + title
			if s := kubeGroupSummary(row.group.key, st); s != "" && !e.open[row.group.key] {
				line += "  " + dimStyle.Render("("+s+")")
			}
			body = kubeLines(body, line)
			if e.open[row.group.key] {
				for _, info := range kubeGroupInfo(row.group.key, st) {
					body = kubeLines(body, kubeHangingIndent("      "+info, inner, 8))
				}
			}
		default:
			f := *row.field
			indent := ""
			if row.group != nil {
				indent = "  "
			}
			body = kubeLines(body, kubeHangingIndent(marker+indent+e.labelCell(f.label+":", i == e.row)+e.fieldValue(m, st, f, i == e.row), inner, kubeEditorValueCol+len(indent)))
			if msg := e.fieldErr[f.path]; msg != "" {
				body = kubeLines(body, kubeHangingIndent("      "+indent+kubeErrStyle.Render(kubeGlyphFailed+" "+msg), inner, 8+len(indent)))
			}
			if f.path == "allow_privileged" {
				body = kubeLines(body, kubeHangingIndent("      "+indent+kubeWarnStyle.Render(kubeGlyphWarn+" "+kubePrivilegedWarning), inner, 8+len(indent)))
			}
			// The ports a pod reaches are the daemon host's, not this
			// machine's, once the daemon is remote.
			if w := fleet.HostGatewayWarning(st.GetCluster().GetHostGateway(), fleetclient.IsRemote()); f.path == "host_gateway" && w != "" {
				body = kubeLines(body, kubeHangingIndent("      "+indent+kubeWarnStyle.Render(kubeGlyphWarn+" "+w), inner, 8+len(indent)))
			}
		}
		if i == e.row {
			cursorStart, cursorEnd = start, len(body)
		}
	}

	var tail []string
	tail = append(tail, "", dimStyle.Render("Changes apply to new instances; Rebuild applies them to existing ones"))
	if e.ns != nil {
		tail = append(tail, "", e.ns.view(m, inner))
	} else {
		tail = append(tail, dialogHint.Render(wrapToWidth(e.footer(m, st), inner)))
	}
	fixed := len(head) + strings.Count(strings.Join(tail, "\n"), "\n") + 1
	visible := kubeScroll(body, cursorStart, cursorEnd, kubeDialogBodyHeight(m, fixed), &e.offset)
	lines := append(append(append([]string(nil), head...), visible...), tail...)
	return kubeDialogFrame(m, strings.Join(lines, "\n"), maxWidth)
}

// The Status row's buttons, by e.button.
const (
	kubeEditorButtonTest = iota + 1
	kubeEditorButtonDeep
	kubeEditorButtonLogin
)

// kubeDeliveryPath is the Delivery field: the Image group's selector
// (kube_delivery.go), not a plain cycling choice.
const kubeDeliveryPath = "image.mode"

// kubeEditorValueCol is the column an editor row's value starts at:
// cursor (2) + label (19) + separator (1).
const kubeEditorValueCol = 22

// labelCell pads a row label to the value column.
func (e *kubeEditor) labelCell(label string, active bool) string {
	cell := fmt.Sprintf("%-19s", label)
	if active {
		return selectedStyle.Render(cell) + " "
	}
	return dialogLabel.Render(cell) + " "
}

// fieldValue renders a field's value: the input while editing it, else its
// value (or what unset means), the row's findings and buttons.
func (e *kubeEditor) fieldValue(m *model, st *fleetgrpc.KubeClusterStatus, f kubeField, active bool) string {
	c := st.GetCluster()
	if e.editing != nil && e.editing.path == f.path {
		return e.input.View()
	}
	text := f.text(c)
	var value string
	switch f.kind {
	case kubeKindChoice, kubeKindTri, kubeKindClass:
		shown := text
		if shown == "" {
			shown = f.def
			if f.kind == kubeKindTri && f.path == "allow_privileged" {
				shown += " → " + onOff(st.GetCapabilities().GetPrivilegedAllowed())
			}
		}
		value = "[ " + shown + " ]"
	case kubeKindBool:
		value = "[ " + text + " ]"
	case kubeKindInt:
		if text == "0" && f.def != "" {
			value = dimStyle.Render("(" + f.def + ")")
		} else {
			value = text
		}
	case kubeKindYAML:
		if text == "" {
			value = dimStyle.Render("(" + f.def + ")")
		} else {
			lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
			value = lines[0]
			if len(lines) > 1 {
				value += dimStyle.Render(fmt.Sprintf(" … %d lines", len(lines)))
			}
		}
	default:
		if text == "" {
			if f.def != "" {
				value = dimStyle.Render("(" + f.def + ")")
			}
		} else {
			value = text
		}
	}
	if f.path == kubeDeliveryPath {
		// [Auto→X] [Registry] [Gateway], Gateway greyed out while the
		// gateway serves no images for this daemon.
		value = kubeDeliverySelector(kubeDeliveryOptions(st))
	}
	if e.saving[f.path] {
		value += " " + m.spinner.View()
	}
	switch f.path {
	case "context":
		if instances, agents := kubeUsers(m, e.id); len(instances)+len(agents) > 0 {
			value += "  " + dimStyle.Render("(re-point: same cluster only)")
		}
	case "namespace":
		if check := kubeCheck(st, "namespace"); check != nil {
			value += "  " + kubeCheckMark(check.GetResult()) + " " + dimStyle.Render(check.GetDetail())
		}
		if kubeNamespaceMissing(st.GetChecks()) {
			value += "  " + kubeButton("[ Create ]", active && e.button == 1)
		}
	}
	if f.note != "" {
		value += "  " + dimStyle.Render(f.note)
	}
	return value
}

// statusValue is the Status row's value: health, version, nodes and when it
// was checked (or the test in flight).
func (e *kubeEditor) statusValue(m *model, st *fleetgrpc.KubeClusterStatus) string {
	var value string
	run := m.kubeRuns[kubeTestKey(e.id)]
	if run.running() {
		value = m.spinner.View() + " " + dimStyle.Render("testing…") + " " + kubeCheckTrail(run.checks, 5)
	} else {
		value = kubeBadge(st)
		caps := st.GetCapabilities()
		var parts []string
		if caps.GetVersion() != "" {
			parts = append(parts, caps.GetVersion())
		}
		if archs := caps.GetNodeArchs(); len(archs) > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", strings.Join(archs, "/"), max(1, len(caps.GetNodes()))))
		}
		if at := st.GetCheckedAt(); at != nil {
			parts = append(parts, "checked "+kubeAgo(kubeNow().Sub(at.AsTime()))+" ago")
		}
		if at := st.GetDeepCheckedAt(); at != nil {
			parts = append(parts, "deep test "+kubeAgo(kubeNow().Sub(at.AsTime()))+" ago")
		}
		if len(parts) > 0 {
			value += " · " + dimStyle.Render(strings.Join(parts, " · "))
		}
	}
	return value
}

// statusLines are the lines under the Status row: a login in flight, a
// test's error, credential expiry.
func (e *kubeEditor) statusLines(m *model, st *fleetgrpc.KubeClusterStatus, inner int) []string {
	var out []string
	if login := m.kubeLogins[e.id]; login != nil {
		switch {
		case !login.done:
			out = append(out, "      "+m.spinner.View()+" "+dimStyle.Render("login on fleetd: "+kubeDeviceCodeLine(login.code)))
		case login.err != nil:
			out = append(out, wrapToWidth("      "+kubeErrStyle.Render(kubeGlyphFailed+" login: "+kubeRPCError(login.err)), inner))
		}
	}
	if run := m.kubeRuns[kubeTestKey(e.id)]; run != nil && run.done && run.err != nil {
		out = append(out, wrapToWidth("      "+kubeErrStyle.Render(kubeGlyphFailed+" test: "+kubeTestError(run.err)), inner))
	}
	if exp := st.GetCapabilities().GetCredentialsExpireAt(); exp != nil {
		// A day out is worth a line; the daemon's own warning comes 30
		// minutes ahead, among the warnings.
		if left := exp.AsTime().Sub(kubeNow()); left > 0 && left <= 24*time.Hour {
			out = append(out, "      "+dimStyle.Render("credentials expire in "+kubeAgo(left)))
		}
	}
	return out
}

// footer lists the keys that do something on the current row.
func (e *kubeEditor) footer(m *model, st *fleetgrpc.KubeClusterStatus) string {
	if e.editing != nil {
		return "[enter] save  [esc] cancel  · " + kubeInputHint(*e.editing)
	}
	row := e.current()
	return kubeFooter(kubeEditorKeys, func(k kubeKey) bool {
		switch {
		case kubeKeyIs(k, "c"):
			return kubeNamespaceMissing(st.GetChecks())
		case kubeKeyIs(k, "L"):
			return kubeLoginHereAvailable() && kubeStatusUsesPlugin(st)
		case kubeKeyIs(k, "left"):
			return row.kind == kubeEditorRowGroup || e.buttons(m, st) > 0 || (row.field != nil && row.field.cycles())
		}
		return true
	})
}

// kubeGroupSummary is a folded group's one-line summary.
func kubeGroupSummary(key string, st *fleetgrpc.KubeClusterStatus) string {
	c := st.GetCluster()
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	switch key {
	case "caches":
		buildkit := "shared buildkit allowed"
		if c.AllowSharedBuildkit != nil && !c.GetAllowSharedBuildkit() {
			buildkit = "shared buildkit off"
		}
		return fmt.Sprintf("apt %s img %s · %s", or(c.GetCacheSizes().GetDebcache(), "10Gi"), or(c.GetCacheSizes().GetImagecache(), "20Gi"), buildkit)
	case "resources":
		parts := []string{or(c.GetResources().GetCpuRequest(), "500m"), or(c.GetResources().GetMemoryRequest(), "2Gi")}
		if c.GetMaxInstances() > 0 {
			parts = append(parts, fmt.Sprintf("max %d", c.GetMaxInstances()))
		}
		if c.GetMaxTotalDisk() != "" {
			parts = append(parts, c.GetMaxTotalDisk())
		}
		runs := "4"
		if c.AutomationMaxPerAgent != nil {
			runs = fmt.Sprint(c.GetAutomationMaxPerAgent())
		}
		return strings.Join(append(parts, runs+" runs/agent"), " · ")
	case "advanced":
		return "pod overrides · extra PATH · plugin env · placement"
	case "capabilities":
		return fmt.Sprintf("%d checks", len(st.GetChecks()))
	}
	return ""
}

// kubeGroupInfo are a group's read-only lines: what the last preflight found
// that bears on its settings.
func kubeGroupInfo(key string, st *fleetgrpc.KubeClusterStatus) []string {
	caps := st.GetCapabilities()
	var out []string
	checkLine := func(name string) {
		if c := kubeCheck(st, name); c != nil {
			line := kubeCheckMark(c.GetResult()) + " " + kubeCheckLabel(c.GetName()) + "  " + dimStyle.Render(c.GetDetail())
			if c.GetFix() != "" {
				line += dimStyle.Render(" · fix: " + c.GetFix())
			}
			out = append(out, line)
		}
	}
	switch key {
	case "capabilities":
		if len(st.GetChecks()) == 0 {
			return []string{dimStyle.Render("not checked yet: [ test ] runs the preflight")}
		}
		// The checks that passed in one line, each one that did not with
		// what it found and the fix.
		var passed []string
		for _, c := range st.GetChecks() {
			if c.GetResult() == fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
				passed = append(passed, kubeCheckMark(c.GetResult())+" "+kubeCheckLabel(c.GetName()))
			}
		}
		if len(passed) > 0 {
			out = append(out, strings.Join(passed, "  "))
		}
		for _, c := range st.GetChecks() {
			if c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
				checkLine(c.GetName())
			}
		}
		for _, w := range st.GetWarnings() {
			out = append(out, kubeWarningLine(w))
		}
		if lr := caps.GetLimitRangeMemoryLimit(); lr != "" {
			limit := st.GetCluster().GetMemoryLimit()
			if limit == "" {
				limit = "8Gi"
			}
			out = append(out, dimStyle.Render(fmt.Sprintf("LimitRange: default memory limit %s → fleet sets %s explicitly", lr, limit)))
		}
		for _, q := range caps.GetQuota() {
			out = append(out, dimStyle.Render(fmt.Sprintf("quota %s: %s of %s used", q.GetResource(), q.GetUsed(), q.GetHard())))
		}
		if o := caps.GetOrphans(); o.GetInstances()+o.GetShares()+o.GetCacheVolumes()+o.GetSnapshotContents() > 0 {
			out = append(out, kubeWarningLine(fmt.Sprintf("orphaned objects: %d instances, %d shares, %d cache volumes (fleet cluster gc lists them)",
				o.GetInstances(), o.GetShares(), o.GetCacheVolumes())))
		}
	case "storage":
		checkLine("root_storage")
		checkLine("snapshots")
		for i, sc := range caps.GetStorageClasses() {
			if i == 6 {
				out = append(out, dimStyle.Render(fmt.Sprintf("… %d more classes", len(caps.GetStorageClasses())-6)))
				break
			}
			out = append(out, dimStyle.Render("class "+kubeStorageClassLine(sc)))
		}
	case "caches":
		checkLine("buildkit")
		if !caps.GetBuildkitAdmitted() && caps.GetBuildkitReason() != "" {
			out = append(out, kubeWarnStyle.Render(kubeGlyphLimited+" shared Buildkit: "+caps.GetBuildkitReason()))
		}
	case "resources":
		checkLine("limits")
		checkLine("quota")
	case "image":
		// A passing delivery check says what the selector and the Auto line
		// already do; one that did not pass names the failing step and fix.
		if c := kubeCheck(st, "image_delivery"); c != nil && c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
			checkLine("image_delivery")
		}
		checkLine("image_pull")
		out = append(out, kubeDeliveryLines(st)...)
	case "security":
		checkLine("admission_unprivileged")
		checkLine("admission_debug_caps")
		checkLine("admission_privileged")
		// The deep test's look at the pods' network: NetworkPolicy
		// enforcement and the metadata block.
		checkLine("network_policy")
		checkLine("metadata")
		if caps.GetPsaLevel() != "" {
			out = append(out, dimStyle.Render("pod-security level: "+caps.GetPsaLevel()))
		}
	}
	return out
}

// kubeStorageClassLine describes a StorageClass as preflight classified it.
func kubeStorageClassLine(sc *fleetgrpc.KubeStorageClass) string {
	var traits []string
	if sc.GetIsDefault() {
		traits = append(traits, "default")
	}
	if sc.GetBlock() {
		traits = append(traits, "block")
	}
	if sc.GetNodeLocal() {
		traits = append(traits, "node-local")
	}
	if sc.GetBindingMode() == "WaitForFirstConsumer" {
		traits = append(traits, "WFFC")
	}
	if sc.GetAllowExpansion() {
		traits = append(traits, "expandable")
	}
	if sc.GetReclaimPolicy() != "" {
		traits = append(traits, sc.GetReclaimPolicy())
	}
	return sc.GetName() + " (" + strings.Join(traits, ", ") + ")"
}
