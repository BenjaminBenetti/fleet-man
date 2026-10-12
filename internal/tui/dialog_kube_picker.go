package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/proto"
)

// dialog_kube_picker.go is the Connect page: a kubectx-style list of the
// contexts of the kubeconfig files on the DAEMON host (fleetd reads them; a
// remote TUI sees the daemon's, never its own), filtered as you type after
// /, and under it the new connection's basic settings (kube_connect_form.go).
// enter on a context chooses it and tests it as an unsaved draft, which also
// finds what the form offers (the cluster's StorageClasses, how images reach
// it); [ Test & connect ] connects it once a test of the form as it stands
// says it is usable, or anyway after one failed. A namespace that does not
// exist is no failure: the daemon creates it as it connects. Everything
// else is the cluster editor's, once connected. The same list re-points an
// existing connection to another context, without the form.

// kubePickerInput is what the picker's text input is collecting.
type kubePickerInput int

const (
	kubePickerInputNone  kubePickerInput = iota
	kubePickerInputPaths                 // ctrl+o: other kubeconfig files on the daemon host
	kubePickerInputField                 // a form field (kubePicker.editing)
)

// kubeDraftSeq numbers the picker's draft tests (their m.kubeRuns keys).
var kubeDraftSeq int

// kubePickerRow is one navigable row: a context, or a form field.
type kubePickerRow struct {
	context *fleetgrpc.KubeContext
	field   string
}

// kubePicker is the Connect page (and the re-point picker).
type kubePicker struct {
	// repoint is the connection being moved to another context; "" when
	// connecting a new one. back is the dialog the picker returns to when it
	// closes (the cluster editor it was opened from), or nil for the list.
	repoint string
	back    kubeDialog

	paths    []string // the kubeconfig files read: what a connection pins
	host     string
	contexts []*fleetgrpc.KubeContext
	loading  bool
	loadErr  string

	cursor    int // index into rows(): the filtered contexts, then the form's
	offset    int
	filter    textinput.Model
	filtering bool

	// chosen is the context the form sets up ("" until enter picks one);
	// forms hold each context's settings and found what its draft tests
	// found, by context name.
	chosen string
	forms  map[string]*kubeConnectForm
	found  map[string]*kubeFound

	input     textinput.Model
	inputMode kubePickerInput
	editing   string // the form field the input edits

	runKey string                 // m.kubeRuns key of the latest draft test
	tested *fleetgrpc.KubeCluster // the draft it tests
	failed bool                   // it finished unusable: [ Connect anyway ]
	// connect: [ Test & connect ] waits for the test to connect it.
	connect bool
	busy    string // an Add or a re-point in flight
	err     string
}

// Answers to the picker's RPCs.
type (
	kubeContextsMsg struct {
		picker *kubePicker
		reply  *fleetgrpc.ListKubeContextsReply
		err    error
	}
	kubeAddedMsg struct {
		picker *kubePicker
		reply  *fleetgrpc.AddKubeClusterReply
		err    error
	}
	kubeRepointedMsg struct {
		picker *kubePicker
		reply  *fleetgrpc.UpdateKubeClusterReply
		err    error
	}
)

func newKubePicker(m *model, repoint string) *kubePicker {
	filter := textinput.New()
	filter.Prompt = ""
	filter.Placeholder = "type to filter"
	filter.CharLimit = 128
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 1024
	p := &kubePicker{repoint: repoint, filter: filter, input: input, forms: map[string]*kubeConnectForm{}, found: map[string]*kubeFound{}}
	if st := m.kubeStatus(repoint); st != nil {
		p.paths = st.GetCluster().GetKubeconfigPaths()
	}
	return p
}

// load lists the contexts of paths (nil: fleetd's default kubeconfig).
func (p *kubePicker) load(m *model, paths []string) tea.Cmd {
	if paths == nil && p.repoint != "" {
		paths = p.paths // a re-point starts from the files the connection pins
	}
	p.loading, p.loadErr = true, ""
	return func() tea.Msg {
		reply, err := listKubeContextsRemote(paths)
		return kubeContextsMsg{picker: p, reply: reply, err: err}
	}
}

// filtered is the contexts the filter matches, in kubeconfig order.
func (p *kubePicker) filtered() []*fleetgrpc.KubeContext {
	q := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	if q == "" {
		return p.contexts
	}
	var out []*fleetgrpc.KubeContext
	for _, c := range p.contexts {
		hay := strings.ToLower(c.GetName() + " " + c.GetServer() + " " + c.GetNamespace() + " " + c.GetCluster())
		if strings.Contains(hay, q) {
			out = append(out, c)
		}
	}
	return out
}

// rows is the page's navigable rows: the filtered contexts, then the form
// of the chosen one (none on a re-point).
func (p *kubePicker) rows() []kubePickerRow {
	var out []kubePickerRow
	for _, c := range p.filtered() {
		out = append(out, kubePickerRow{context: c})
	}
	if c := p.chosenContext(); c != nil && p.repoint == "" {
		for _, f := range p.formFields(c) {
			out = append(out, kubePickerRow{field: f})
		}
	}
	return out
}

// current is the row under the cursor (the zero row when there is none).
func (p *kubePicker) current() kubePickerRow {
	rows := p.rows()
	if len(rows) == 0 {
		return kubePickerRow{}
	}
	p.cursor = max(0, min(p.cursor, len(rows)-1))
	return rows[p.cursor]
}

// selected is the context under the cursor, or nil on a form row.
func (p *kubePicker) selected() *fleetgrpc.KubeContext {
	return p.current().context
}

// chosenContext is the context the form sets up, or nil.
func (p *kubePicker) chosenContext() *fleetgrpc.KubeContext {
	if p.chosen == "" {
		return nil
	}
	for _, c := range p.contexts {
		if c.GetName() == p.chosen {
			return c
		}
	}
	return nil
}

// target is the context a key acts on: the one under the cursor, else (on
// the form) the chosen one.
func (p *kubePicker) target() *fleetgrpc.KubeContext {
	if c := p.selected(); c != nil {
		return c
	}
	return p.chosenContext()
}

// cursorTo puts the cursor on a form field, or on context name ("" field).
func (p *kubePicker) cursorTo(field, name string) {
	for i, row := range p.rows() {
		if (field != "" && row.field == field) || (field == "" && row.context.GetName() == name) {
			p.cursor = i
			return
		}
	}
}

// kubeNameMaxBytes is the longest display name the daemon accepts.
const kubeNameMaxBytes = 63

// kubeDefaultName names a new connection after its context, or after its
// context and namespace when that is taken (names are unique per daemon). A
// long context (an EKS ARN) is shortened in the middle to the daemon's limit.
func kubeDefaultName(m *model, context, ns string) string {
	taken := map[string]bool{}
	for _, st := range m.kubeStatuses() {
		taken[strings.ToLower(st.GetCluster().DisplayNameOrID())] = true
	}
	for _, name := range []string{context, context + "/" + ns} {
		if strings.ContainsFunc(name, unicode.IsControl) {
			break // refused by the daemon: let it name the connection
		}
		name = kubeFitBytes(name, kubeNameMaxBytes)
		if !taken[strings.ToLower(name)] {
			return name
		}
	}
	return "" // the daemon names it after its id
}

// kubeFitBytes is s shortened in the middle (kubeShortContext) until it is
// at most max bytes: the limit is in bytes, and the ellipsis alone is three.
func kubeFitBytes(s string, max int) string {
	out := s
	for n := utf8.RuneCountInString(s) - 1; len(out) > max && n > 0; n-- {
		out = kubeShortContext(s, n)
	}
	return out
}

// run is the latest draft test, or nil.
func (p *kubePicker) run(m *model) *kubeTestRun {
	if p.runKey == "" {
		return nil
	}
	return m.kubeRuns[p.runKey]
}

// stopTest cancels the draft test.
func (p *kubePicker) stopTest(m *model) {
	if run := p.run(m); run != nil {
		run.cancel()
		delete(m.kubeRuns, p.runKey)
	}
	p.runKey, p.tested, p.connect = "", nil, false
}

// kubeDraftKey is what a draft test depends on: everything but the name.
func kubeDraftKey(d *fleetgrpc.KubeCluster) string {
	c := proto.Clone(d).(*fleetgrpc.KubeCluster)
	c.DisplayName = ""
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return string(b)
}

// sameDraft reports whether the latest test was of context c's draft as
// the form now stands.
func (p *kubePicker) sameDraft(m *model, c *fleetgrpc.KubeContext) bool {
	return p.runKey != "" && p.tested != nil && c != nil && kubeDraftKey(p.tested) == kubeDraftKey(p.draft(m, c))
}

func (p *kubePicker) update(m *model, page *settingsPage, msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case kubeContextsMsg:
		if msg.picker != p {
			return nil, false
		}
		p.onContexts(m, msg)
		return nil, true
	case kubeAddedMsg:
		if msg.picker != p {
			return nil, false
		}
		return p.onAdded(m, page, msg), true
	case kubeRepointedMsg:
		if msg.picker != p {
			return nil, false
		}
		p.busy = ""
		if msg.err != nil {
			p.err = "re-point: " + kubeRPCError(msg.err)
			return nil, true
		}
		st := msg.reply.GetCluster()
		m.kubeUpsert(st)
		page.closeKubeDialog(m)
		page.cursorToKubeCluster(m, st.GetCluster().GetId())
		m.message = fmt.Sprintf("%s %s now uses context %s", kubeGlyphCluster, st.GetCluster().DisplayNameOrID(), st.GetCluster().GetContext())
		if e, ok := page.kubeDialog.(*kubeEditor); ok {
			e.onNamespace(m, msg.reply.GetNamespace())
		}
		return nil, true
	case kubeLoginHereDoneMsg:
		if msg.owner != kubeDialog(p) {
			return nil, false
		}
		if msg.err != nil {
			p.err = "login: " + msg.err.Error()
			return nil, true
		}
		m.message = "Login plugin finished; testing again"
		if c := p.chosenContext(); c != nil && p.repoint == "" {
			return p.startTest(m, c, p.connect), true
		}
		return nil, true
	case tea.KeyMsg:
		return p.key(m, page, msg), true
	}
	return nil, false
}

// onContexts applies a ListKubeContexts answer. The cursor starts on the
// kubeconfig's current context (on a re-point, on the connection's own).
func (p *kubePicker) onContexts(m *model, msg kubeContextsMsg) {
	p.loading = false
	if msg.err != nil {
		p.loadErr = kubeRPCError(msg.err)
		return
	}
	p.contexts = msg.reply.GetContexts()
	p.paths = msg.reply.GetKubeconfigPaths()
	p.host = msg.reply.GetHost()
	if p.host != "" {
		m.kubeHost = p.host
	}
	p.cursor = 0
	want := ""
	if st := m.kubeStatus(p.repoint); st != nil {
		want = st.GetCluster().GetContext()
	}
	for i, c := range p.contexts {
		if (want != "" && c.GetName() == want) || (want == "" && c.GetCurrent()) {
			p.cursor = i
			break
		}
	}
}

// onAdded closes the page on a new connection, saying what became of its
// namespace: one the daemon could not create opens the connection's editor
// on the namespace panel, with the manifest to apply.
func (p *kubePicker) onAdded(m *model, page *settingsPage, msg kubeAddedMsg) tea.Cmd {
	p.busy = ""
	if msg.err != nil {
		p.err = "connect: " + kubeRPCError(msg.err)
		return nil
	}
	st := msg.reply.GetCluster()
	id := st.GetCluster().GetId()
	m.kubeUpsert(st)
	page.closeKubeDialog(m)
	page.cursorToKubeCluster(m, id)
	m.message = fmt.Sprintf("Connected %s %s: %s", kubeGlyphCluster, st.GetCluster().DisplayNameOrID(), kubeStateLine(st))
	cmds := []tea.Cmd{loadKubePluginsCmd(m.watchGen, m.kubeStatuses())}
	switch ns := msg.reply.GetNamespace(); {
	case ns.GetPermissionDenied():
		e := newKubeEditor(id)
		e.gen = m.watchGen
		e.ns = newKubeNamespaceRefusal(id, ns)
		page.kubeDialog = e
		m.message = fmt.Sprintf("Connected %s %s, but fleet may not create namespace %s: apply its manifest as someone who may",
			kubeGlyphCluster, st.GetCluster().DisplayNameOrID(), ns.GetNamespace())
		cmds = append(cmds, e.loadUsage())
	case ns.GetCreated():
		m.message += " · created namespace " + ns.GetNamespace()
	case ns.GetError() != "":
		m.message += " · namespace " + ns.GetNamespace() + ": " + ns.GetError()
	}
	return tea.Batch(cmds...)
}

func (p *kubePicker) key(m *model, page *settingsPage, key tea.KeyMsg) tea.Cmd {
	if p.inputMode != kubePickerInputNone {
		return p.inputKey(m, key)
	}
	if p.filtering {
		switch key.String() {
		case "esc", "enter":
			p.filtering = false
			p.filter.Blur()
			return nil
		case "up", "down":
			p.move(key.String())
			return nil
		}
		before := p.filter.Value()
		var cmd tea.Cmd
		p.filter, cmd = p.filter.Update(key)
		if p.filter.Value() != before {
			p.cursor, p.offset = 0, 0
		}
		return cmd
	}
	row := p.current()
	switch key.String() {
	case "esc", "q":
		page.closeKubeDialog(m)
		return nil
	case "j", "k", "up", "down":
		p.move(key.String())
	case "tab", "shift+tab":
		p.toggleFocus()
	case "/":
		p.filtering = true
		return p.filter.Focus()
	case "enter":
		if row.context != nil {
			if p.repoint != "" {
				return p.repointTo(m, row.context)
			}
			return p.choose(m, row.context, true)
		}
		return p.enterField(m, row.field)
	case "left", "h", "right", "l":
		if row.field != "" {
			dir := 1
			if key.String() == "left" || key.String() == "h" {
				dir = -1
			}
			return p.cycleField(m, row.field, dir)
		}
	case "n":
		if c := p.target(); c != nil && p.repoint == "" {
			p.choose(m, c, false)
			p.cursorTo(kubeFormNamespace, "")
			return p.enterField(m, kubeFormNamespace)
		}
	case "ctrl+o":
		return p.beginInput(kubePickerInputPaths, "", strings.Join(p.paths, string(filepath.ListSeparator)),
			"/abs/path/to/kubeconfig on fleetd's host ('"+string(filepath.ListSeparator)+"' separates files)")
	case "L":
		if c := p.target(); c != nil && kubeLoginHereAvailable() && kubeUsesPlugin(c) {
			return kubeLoginHereCmd(p, p.draft(m, c))
		}
	}
	return nil
}

// move steps the cursor through the rows, wrapping.
func (p *kubePicker) move(key string) {
	n := len(p.rows())
	if n == 0 {
		return
	}
	switch key {
	case "j", "down":
		p.cursor = (p.cursor + 1) % n
	default:
		p.cursor = (p.cursor - 1 + n) % n
	}
}

// toggleFocus jumps between the contexts and the form: to the form's first
// field, or back to the chosen context.
func (p *kubePicker) toggleFocus() {
	c := p.chosenContext()
	if c == nil || p.repoint != "" {
		return
	}
	if p.current().context != nil {
		p.cursorTo(kubeFormName, "")
		return
	}
	p.cursorTo("", c.GetName())
}

// choose makes c the context the form sets up, its cursor on [ Test &
// connect ], and with test tests the draft (unless the latest test was of
// it already), which is also what fills the form's choices.
func (p *kubePicker) choose(m *model, c *fleetgrpc.KubeContext, test bool) tea.Cmd {
	if p.chosen != c.GetName() {
		p.connect = false
	}
	p.chosen, p.err = c.GetName(), ""
	p.cursorTo(kubeFormConnect, "")
	if !test || p.sameDraft(m, c) {
		return nil
	}
	return p.startTest(m, c, false)
}

func (p *kubePicker) beginInput(mode kubePickerInput, field, value, placeholder string) tea.Cmd {
	p.inputMode, p.editing = mode, field
	p.input.Placeholder = placeholder
	p.input.SetValue(value)
	p.input.CursorEnd()
	return p.input.Focus()
}

func (p *kubePicker) inputKey(m *model, key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		p.inputMode, p.editing = kubePickerInputNone, ""
		p.input.Blur()
		return nil
	case "enter":
		mode, field := p.inputMode, p.editing
		value := strings.TrimSpace(p.input.Value())
		p.inputMode, p.editing = kubePickerInputNone, ""
		p.input.Blur()
		switch mode {
		case kubePickerInputPaths:
			var paths []string
			for _, part := range strings.Split(value, string(filepath.ListSeparator)) {
				if part = strings.TrimSpace(part); part != "" {
					paths = append(paths, part)
				}
			}
			// Other files: other contexts, which nothing found yet.
			p.stopTest(m)
			p.contexts, p.filter, p.err = nil, resetInput(p.filter), ""
			p.chosen, p.forms, p.found, p.failed = "", map[string]*kubeConnectForm{}, map[string]*kubeFound{}, false
			return p.load(m, paths)
		case kubePickerInputField:
			return p.setField(m, field, value)
		}
		return nil
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(key)
	return cmd
}

// kubeUsesPlugin reports whether context c logs in through an exec
// credential plugin, the only kind there is a login to run for.
func kubeUsesPlugin(c *fleetgrpc.KubeContext) bool {
	return c.GetAuth() == "exec-plugin" || c.GetPlugin() != ""
}

// resetInput clears a text input.
func resetInput(in textinput.Model) textinput.Model {
	in.SetValue("")
	in.Blur()
	return in
}

// connectChosen is [ Test & connect ]: connect the chosen context once a
// test of its draft as it stands says it is usable (testing it first when
// none has), or anyway after one failed.
func (p *kubePicker) connectChosen(m *model) tea.Cmd {
	c := p.chosenContext()
	if c == nil || p.busy != "" {
		return nil
	}
	p.err = ""
	switch run := p.run(m); {
	case !p.sameDraft(m, c):
		return p.startTest(m, c, true)
	case run.running():
		p.connect = true
		return nil
	}
	// Usable, or a failed test the user connects anyway.
	return p.add(m, p.draft(m, c))
}

// repointTo moves the re-pointed connection to context c (the daemon
// refuses another cluster for a connection in use).
func (p *kubePicker) repointTo(m *model, c *fleetgrpc.KubeContext) tea.Cmd {
	if p.busy != "" {
		return nil
	}
	p.err = ""
	st := m.kubeStatus(p.repoint)
	if st == nil {
		p.err = "the connection is gone"
		return nil
	}
	p.busy = "re-pointing…"
	id, rev := p.repoint, st.GetCluster().GetRevision()
	next := &fleetgrpc.KubeCluster{Context: c.GetName(), KubeconfigPaths: append([]string(nil), p.paths...)}
	return func() tea.Msg {
		reply, err := updateKubeClusterRemote(id, next, []string{"context", "kubeconfig_paths"}, rev)
		return kubeRepointedMsg{picker: p, reply: reply, err: err}
	}
}

// startTest runs the preflight on context c's draft without saving
// anything; connect connects it when the test says it is usable.
func (p *kubePicker) startTest(m *model, c *fleetgrpc.KubeContext, connect bool) tea.Cmd {
	p.stopTest(m)
	kubeDraftSeq++
	p.runKey = fmt.Sprintf("draft:%d", kubeDraftSeq)
	p.tested = p.draft(m, c)
	p.failed, p.connect = false, connect
	return m.startKubeTest(p.runKey, "", &fleetgrpc.TestKubeClusterRequest{Cluster: proto.Clone(p.tested).(*fleetgrpc.KubeCluster)})
}

// kubeNamespaceToCreate reports whether a draft test failed only because its
// namespace does not exist, which the daemon creates as it connects.
func kubeNamespaceToCreate(checks []*fleetgrpc.KubeCheck) bool {
	failed := false
	for _, c := range checks {
		if c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL {
			continue
		}
		if c.GetName() != "namespace" {
			return false
		}
		failed = true
	}
	return failed
}

// kubeDraftUsable reports whether a finished draft test lets the draft
// connect without a "connect anyway".
func kubeDraftUsable(run *kubeTestRun) bool {
	return run.err == nil && (run.result.GetState().Usable() || kubeNamespaceToCreate(run.checks))
}

// onTestDone keeps what the test found for the form's choices, then
// connects a usable draft [ Test & connect ] waits for, or marks it failed.
func (p *kubePicker) onTestDone(m *model, run *kubeTestRun) tea.Cmd {
	if p.tested != nil && run.result != nil {
		// What it found can add a row (Node-local OK): the cursor stays on
		// its row.
		row := p.current()
		p.foundFor(p.tested.GetContext()).take(p.tested, run.result, run.checks)
		p.cursorTo(row.field, row.context.GetName())
	}
	c := p.chosenContext()
	if !p.sameDraft(m, c) {
		return nil
	}
	usable := kubeDraftUsable(run)
	p.failed = !usable
	if p.connect {
		p.connect = false
		if usable {
			return p.add(m, p.draft(m, c))
		}
	}
	return nil
}

// add saves the draft as a connection.
func (p *kubePicker) add(m *model, draft *fleetgrpc.KubeCluster) tea.Cmd {
	p.busy = "connecting…"
	return func() tea.Msg {
		reply, err := addKubeClusterRemote(draft)
		return kubeAddedMsg{picker: p, reply: reply, err: err}
	}
}

func (p *kubePicker) view(m *model, _ *settingsPage) string {
	const maxWidth = 110
	inner := kubeDialogInner(m, maxWidth)
	var head, body, tail []string

	title := "Connect a Kubernetes cluster"
	if st := m.kubeStatus(p.repoint); st != nil {
		title = fmt.Sprintf("Re-point %s %s", kubeGlyphCluster, st.GetCluster().DisplayNameOrID())
	}
	head = append(head, dialogTitle.Render(title))
	// The host and the files read share a line when they fit; otherwise the
	// files get a line of their own, so a long host name never cuts into the
	// path that was just typed.
	where := "contexts on fleetd"
	if p.host != "" {
		where += " @ " + p.host
	}
	if paths := strings.Join(p.paths, ", "); paths != "" {
		if line := where + " · " + paths; utf8.RuneCountInString(line) <= inner {
			where = line
		} else {
			head = append(head, dimStyle.Render(kubeShortContext(where, inner)))
			where = paths
		}
	}
	head = append(head, dimStyle.Render(kubeShortContext(where, inner)))
	switch {
	case p.inputMode == kubePickerInputPaths:
		head = append(head, dialogLabel.Render("Kubeconfig: ")+p.input.View())
	case p.filtering || p.filter.Value() != "":
		head = append(head, dialogLabel.Render("Filter: ")+p.filter.View())
	default:
		head = append(head, dialogLabel.Render("Filter: ")+dimStyle.Render("/ to filter"))
	}

	nameW := max(12, min(30, inner/3))
	serverW := max(12, min(30, inner/3))
	head = append(head, dimStyle.Render(fmt.Sprintf("    %-*s %-*s %s", nameW, "CONTEXT", serverW, "SERVER", "NAMESPACE")))

	list := p.filtered()
	rows := p.rows()
	p.cursor = max(0, min(p.cursor, len(rows)-1))
	cursorStart, cursorEnd := 0, 0
	switch {
	case p.loading:
		body = kubeLines(body, "  "+m.spinner.View()+" "+dimStyle.Render("reading the kubeconfig on fleetd…"))
	case p.loadErr != "":
		body = kubeLines(body, wrapToWidth("  "+kubeErrStyle.Render(kubeGlyphFailed+" "+p.loadErr), inner))
	case len(p.contexts) == 0:
		body = kubeLines(body, "  "+dimStyle.Render("no contexts in this kubeconfig · ctrl+o reads another file"))
	case len(list) == 0:
		body = kubeLines(body, "  "+dimStyle.Render("no context matches the filter"))
	}
	for i, c := range list {
		marker := "  "
		switch {
		case i == p.cursor:
			marker = cursorStyle.Render("> ")
			cursorStart = len(body)
		case c.GetName() == p.chosen && p.repoint == "":
			// The context the form below sets up.
			marker = cursorStyle.Render("▸ ")
		}
		flag := "  "
		switch {
		case len(c.GetConnected()) > 0:
			flag = kubeOKStyle.Render(kubeGlyphPass) + " "
		case c.GetCurrent():
			flag = "* "
		}
		name := fmt.Sprintf("%-*s", nameW, kubeShortContext(c.GetName(), nameW))
		if i == p.cursor || (c.GetName() == p.chosen && p.repoint == "") {
			name = selectedStyle.Render(name)
		}
		ns := c.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		body = kubeLines(body, marker+flag+name+" "+dimStyle.Render(fmt.Sprintf("%-*s", serverW, kubeShortContext(c.GetServer(), serverW)))+" "+ns)
		if c.GetPluginMissing() != "" {
			line := strings.Repeat(" ", 5+nameW) + dimStyle.Render("exec: "+filepath.Base(c.GetPlugin())+" ") +
				kubeWarnStyle.Render(kubeGlyphWarn+" "+c.GetPluginMissing())
			body = kubeLines(body, kubeHangingIndent(line, inner, 5+nameW))
		}
		if i == p.cursor {
			cursorEnd = len(body)
		}
	}

	switch {
	case p.repoint != "":
		if p.busy != "" {
			tail = append(tail, m.spinner.View()+" "+dimStyle.Render(p.busy))
		}
	case len(p.contexts) > 0:
		tail = append(tail, "")
		tail = append(tail, p.formLines(m, rows, inner)...)
	}
	if p.err != "" {
		tail = append(tail, wrapToWidth(kubeErrStyle.Render(kubeGlyphFailed+" "+p.err), inner))
	}
	tail = append(tail, dialogHint.Render(wrapToWidth(p.footer(m), inner)))

	fixed := len(head) + strings.Count(strings.Join(tail, "\n"), "\n") + 1
	visible := kubeScroll(body, cursorStart, max(cursorEnd, cursorStart+1), kubeDialogBodyHeight(m, fixed), &p.offset)
	lines := append(append(append([]string(nil), head...), visible...), tail...)
	return kubeDialogFrame(m, strings.Join(lines, "\n"), maxWidth)
}

// statusLines describe the chosen context's draft test: in flight, or its
// result with what failed and what can be done next.
func (p *kubePicker) statusLines(m *model, c *fleetgrpc.KubeContext, inner int, onConnect bool) []string {
	if p.busy != "" {
		return []string{m.spinner.View() + " " + dimStyle.Render(p.busy)}
	}
	run := p.run(m)
	if run == nil || !p.sameDraft(m, c) {
		return nil
	}
	var out []string
	lead := "  " + dialogLabel.Render("Test:") + "  "
	switch {
	case run.running():
		out = append(out, kubeHangingIndent(lead+m.spinner.View()+" "+dimStyle.Render("testing…")+" "+kubeCheckTrail(run.checks, 8), inner, 4))
	case run.err != nil:
		out = append(out, kubeHangingIndent(lead+kubeErrStyle.Render(kubeGlyphFailed+" "+kubeTestError(run.err)), inner, 4))
	default:
		badge, checks := kubeBadge(run.result), run.checks
		if kubeNamespaceToCreate(run.checks) {
			// Not a failure: the daemon creates it as it connects.
			ns, _ := p.namespaceFor(c)
			badge = kubeWarnStyle.Render(kubeGlyphLimited + " namespace " + ns + " to create")
			checks = slices.DeleteFunc(slices.Clone(checks), func(c *fleetgrpc.KubeCheck) bool { return c.GetName() == "namespace" })
		}
		out = append(out, kubeHangingIndent(lead+badge+"  "+kubeCheckSummary(checks), inner, 4))
		for _, check := range run.checks {
			if check.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL {
				continue
			}
			line := "  " + check.GetDetail()
			if check.GetName() == "namespace" {
				line = "  " + dimStyle.Render(check.GetDetail()+": fleet creates it when it connects")
			} else if check.GetFix() != "" {
				line += dimStyle.Render(" · " + check.GetFix())
			}
			out = append(out, kubeHangingIndent(line, inner, 4))
		}
	}
	if !run.running() && p.failed {
		hint := "connect anyway with [ Connect anyway ]"
		if onConnect {
			hint = "[enter] connect anyway"
		}
		out = append(out, dimStyle.Render("  "+hint+" (Settings shows its health; fix and test it there)"))
	}
	return out
}

// footer lists the keys that do something on the current row.
func (p *kubePicker) footer(m *model) string {
	switch {
	case p.inputMode != kubePickerInputNone:
		return "[enter] apply  [esc] cancel"
	case p.filtering:
		return "type to filter  [↑/↓] move  [enter/esc] done"
	}
	row := p.current()
	c := p.target()
	footer := kubeFooter(kubePickerKeys, func(k kubeKey) bool {
		switch {
		case kubeKeyIs(k, "L"):
			return c != nil && kubeLoginHereAvailable() && kubeUsesPlugin(c)
		case kubeKeyIs(k, "n"):
			return c != nil && p.repoint == ""
		case kubeKeyIs(k, "tab"):
			return p.chosenContext() != nil && p.repoint == ""
		case kubeKeyIs(k, "left"):
			return kubeFormCycles(row.field)
		case kubeKeyIs(k, "/"), kubeKeyIs(k, "ctrl+o"):
			return row.field == ""
		}
		return true
	})
	switch {
	case p.repoint != "":
		footer = strings.Replace(footer, kubePickerConnect.hint, "[enter] re-point here", 1)
	case row.field == kubeFormConnect:
		footer = strings.Replace(footer, kubePickerConnect.hint, "[enter] "+strings.ToLower(strings.Trim(p.connectLabel(m), "[ ]")), 1)
	case kubeFormCycles(row.field):
		footer = strings.Replace(footer, kubePickerConnect.hint+"  ", "", 1)
	case row.field != "":
		footer = strings.Replace(footer, kubePickerConnect.hint, "[enter] edit", 1)
	}
	if row.field != "" {
		footer = strings.Replace(footer, "[tab] form", "[tab] contexts", 1)
	}
	return footer
}
