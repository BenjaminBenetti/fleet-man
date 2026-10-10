package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// dialog_kube_picker.go is the kubectx-style context picker: the contexts of
// the kubeconfig files on the DAEMON host (fleetd reads them; a remote TUI
// sees the daemon's, never its own), filtered as you type after /. Enter
// tests the context with a streamed preflight and connects it when it is
// usable (a second enter connects a failing one anyway). The same picker
// re-points an existing connection to another context.

// kubePickerInput is what the picker's text input is collecting.
type kubePickerInput int

const (
	kubePickerInputNone      kubePickerInput = iota
	kubePickerInputPaths                     // ctrl+o: other kubeconfig files on the daemon host
	kubePickerInputNamespace                 // n: the namespace to connect
)

// kubeDraftSeq numbers the picker's draft tests (their m.kubeRuns keys).
var kubeDraftSeq int

// kubePicker is the context picker dialog.
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

	cursor    int // index into filtered()
	offset    int
	filter    textinput.Model
	filtering bool

	input      textinput.Model
	inputMode  kubePickerInput
	namespaces map[string]string // a namespace chosen with n, by context

	runKey  string // m.kubeRuns key of the latest draft test
	testCtx string // the context and namespace it tested
	testNS  string
	failed  bool   // it finished unusable: enter again connects anyway
	busy    string // an Add or a re-point in flight
	err     string
	ns      *kubeNamespacePanel
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
		status *fleetgrpc.KubeClusterStatus
		err    error
	}
	kubeRepointedMsg struct {
		picker *kubePicker
		status *fleetgrpc.KubeClusterStatus
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
	p := &kubePicker{repoint: repoint, filter: filter, input: input, namespaces: map[string]string{}}
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

// selected is the context under the cursor, or nil.
func (p *kubePicker) selected() *fleetgrpc.KubeContext {
	list := p.filtered()
	if p.cursor < 0 || p.cursor >= len(list) {
		return nil
	}
	return list[p.cursor]
}

// namespaceFor is the namespace a connection of context c takes: one chosen
// with n, else the context's own, except that a context on "default" (or on
// none) is offered "fleet" instead, created only through the explicit Create.
func (p *kubePicker) namespaceFor(c *fleetgrpc.KubeContext) (ns string, suggested bool) {
	if ns := p.namespaces[c.GetName()]; ns != "" {
		return ns, false
	}
	if ns := c.GetNamespace(); ns != "" && ns != "default" {
		return ns, false
	}
	return "fleet", true
}

// draft is the unsaved connection of context c.
func (p *kubePicker) draft(m *model, c *fleetgrpc.KubeContext) *fleetgrpc.KubeCluster {
	ns, _ := p.namespaceFor(c)
	return &fleetgrpc.KubeCluster{
		DisplayName:     kubeDefaultName(m, c.GetName(), ns),
		Context:         c.GetName(),
		KubeconfigPaths: append([]string(nil), p.paths...),
		Namespace:       ns,
	}
}

// kubeDefaultName names a new connection after its context, or after its
// context and namespace when that is taken (names are unique per daemon).
func kubeDefaultName(m *model, context, ns string) string {
	taken := map[string]bool{}
	for _, st := range m.kubeStatuses() {
		taken[strings.ToLower(kubeDisplayName(st.GetCluster()))] = true
	}
	for _, name := range []string{context, context + "/" + ns} {
		name = kubeShortContext(name, 63)
		if !taken[strings.ToLower(name)] {
			return name
		}
	}
	return "" // the daemon names it after its id
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
	p.runKey = ""
}

// sameDraft reports whether the latest test was of the context and
// namespace now selected.
func (p *kubePicker) sameDraft(c *fleetgrpc.KubeContext) bool {
	ns, _ := p.namespaceFor(c)
	return p.runKey != "" && p.testCtx == c.GetName() && p.testNS == ns
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
		p.busy = ""
		if msg.err != nil {
			p.err = "connect: " + kubeRPCError(msg.err)
			return nil, true
		}
		m.kubeUpsert(msg.status)
		page.closeKubeDialog(m)
		page.cursorToKubeCluster(m, msg.status.GetCluster().GetId())
		m.message = fmt.Sprintf("Connected %s %s: %s", kubeGlyphCluster, kubeDisplayName(msg.status.GetCluster()), kubeStateLine(msg.status))
		return loadKubePluginsCmd(m.watchGen, m.kubeStatuses()), true
	case kubeRepointedMsg:
		if msg.picker != p {
			return nil, false
		}
		p.busy = ""
		if msg.err != nil {
			p.err = "re-point: " + kubeRPCError(msg.err)
			return nil, true
		}
		m.kubeUpsert(msg.status)
		page.closeKubeDialog(m)
		page.cursorToKubeCluster(m, msg.status.GetCluster().GetId())
		m.message = fmt.Sprintf("%s %s now uses context %s", kubeGlyphCluster, kubeDisplayName(msg.status.GetCluster()), msg.status.GetCluster().GetContext())
		return nil, true
	case kubeNamespaceMsg:
		if p.ns == nil || msg.panel != p.ns {
			return nil, false
		}
		if p.ns.handle(msg) {
			p.ns = nil
			m.message = "Namespace " + msg.panel.name + " created"
			if c := p.selected(); c != nil && p.repoint == "" {
				return p.startTest(m, c), true
			}
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
		if c := p.selected(); c != nil && p.repoint == "" {
			return p.startTest(m, c), true
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

func (p *kubePicker) key(m *model, page *settingsPage, key tea.KeyMsg) tea.Cmd {
	if p.ns != nil {
		cmd, closed := p.ns.key(m, key)
		if closed {
			p.ns = nil
		}
		return cmd
	}
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
	switch key.String() {
	case "esc", "q":
		page.closeKubeDialog(m)
		return nil
	case "j", "k", "up", "down":
		p.move(key.String())
	case "/":
		p.filtering = true
		return p.filter.Focus()
	case "enter":
		return p.connect(m)
	case "n":
		if c := p.selected(); c != nil && p.repoint == "" {
			ns, _ := p.namespaceFor(c)
			return p.beginInput(kubePickerInputNamespace, ns, "namespace (lower-case letters, digits, '-')")
		}
	case "c":
		if c := p.selected(); c != nil && p.namespaceMissing(m, c) {
			var cmd tea.Cmd
			ns, _ := p.namespaceFor(c)
			p.ns, cmd = newKubeNamespacePanel("", p.draft(m, c), ns)
			return cmd
		}
	case "ctrl+o":
		return p.beginInput(kubePickerInputPaths, strings.Join(p.paths, string(filepath.ListSeparator)),
			"/abs/path/to/kubeconfig on fleetd's host ('"+string(filepath.ListSeparator)+"' separates files)")
	case "L":
		if c := p.selected(); c != nil && kubeLoginHereAvailable() && kubeUsesPlugin(c) {
			return kubeLoginHereCmd(p, p.draft(m, c))
		}
	}
	return nil
}

// move steps the cursor through the filtered contexts.
func (p *kubePicker) move(key string) {
	n := len(p.filtered())
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

func (p *kubePicker) beginInput(mode kubePickerInput, value, placeholder string) tea.Cmd {
	p.inputMode = mode
	p.input.Placeholder = placeholder
	p.input.SetValue(value)
	p.input.CursorEnd()
	return p.input.Focus()
}

func (p *kubePicker) inputKey(m *model, key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		p.inputMode = kubePickerInputNone
		p.input.Blur()
		return nil
	case "enter":
		mode := p.inputMode
		value := strings.TrimSpace(p.input.Value())
		p.inputMode = kubePickerInputNone
		p.input.Blur()
		switch mode {
		case kubePickerInputPaths:
			var paths []string
			for _, part := range strings.Split(value, string(filepath.ListSeparator)) {
				if part = strings.TrimSpace(part); part != "" {
					paths = append(paths, part)
				}
			}
			p.contexts, p.filter, p.err = nil, resetInput(p.filter), ""
			p.stopTest(m)
			return p.load(m, paths)
		case kubePickerInputNamespace:
			if c := p.selected(); c != nil {
				p.namespaces[c.GetName()] = value
			}
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

// namespaceMissing reports whether the latest test of c found its namespace
// absent.
func (p *kubePicker) namespaceMissing(m *model, c *fleetgrpc.KubeContext) bool {
	run := p.run(m)
	return p.sameDraft(c) && run != nil && kubeNamespaceMissing(run.checks)
}

// connect is enter: test the selected context, then connect it when the
// test says it is usable; after a failed test, connect it anyway. On a
// re-point it moves the connection there (the daemon refuses another cluster
// for a connection in use).
func (p *kubePicker) connect(m *model) tea.Cmd {
	c := p.selected()
	if c == nil || p.busy != "" {
		return nil
	}
	p.err = ""
	if p.repoint != "" {
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
			return kubeRepointedMsg{picker: p, status: reply, err: err}
		}
	}
	if run := p.run(m); p.sameDraft(c) && run.running() {
		return nil
	}
	if p.sameDraft(c) && p.failed {
		return p.add(m, p.draft(m, c))
	}
	return p.startTest(m, c)
}

// startTest runs the preflight on context c without saving anything.
func (p *kubePicker) startTest(m *model, c *fleetgrpc.KubeContext) tea.Cmd {
	p.stopTest(m)
	kubeDraftSeq++
	p.runKey = fmt.Sprintf("draft:%d", kubeDraftSeq)
	p.testCtx = c.GetName()
	p.testNS, _ = p.namespaceFor(c)
	p.failed = false
	return m.startKubeTest(p.runKey, "", &fleetgrpc.TestKubeClusterRequest{Cluster: p.draft(m, c)})
}

// onTestDone connects a usable draft, or says what failed.
func (p *kubePicker) onTestDone(m *model, run *kubeTestRun) tea.Cmd {
	c := p.selected()
	if c == nil || c.GetName() != p.testCtx {
		return nil // the cursor moved on; the result stays for when it is back
	}
	if run.err == nil && kubeUsable(run.result.GetState()) {
		return p.add(m, p.draft(m, c))
	}
	p.failed = true
	return nil
}

// add saves the draft as a connection.
func (p *kubePicker) add(m *model, draft *fleetgrpc.KubeCluster) tea.Cmd {
	p.busy = "connecting…"
	return func() tea.Msg {
		st, err := addKubeClusterRemote(draft)
		return kubeAddedMsg{picker: p, status: st, err: err}
	}
}

func (p *kubePicker) view(m *model, _ *settingsPage) string {
	const maxWidth = 110
	inner := kubeDialogInner(m, maxWidth)
	var head, body, tail []string

	title := "Connect a Kubernetes cluster"
	if st := m.kubeStatus(p.repoint); st != nil {
		title = fmt.Sprintf("Re-point %s %s", kubeGlyphCluster, kubeDisplayName(st.GetCluster()))
	}
	head = append(head, dialogTitle.Render(title))
	where := "contexts on fleetd"
	if p.host != "" {
		where += " @ " + p.host
	}
	if len(p.paths) > 0 {
		where += " · " + strings.Join(p.paths, ", ")
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
		if i == p.cursor {
			marker = cursorStyle.Render("> ")
			cursorStart = len(body)
		}
		flag := "  "
		switch {
		case len(c.GetConnected()) > 0:
			flag = kubeOKStyle.Render(kubeGlyphPass) + " "
		case c.GetCurrent():
			flag = "* "
		}
		name := fmt.Sprintf("%-*s", nameW, kubeShortContext(c.GetName(), nameW))
		if i == p.cursor {
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

	if c := p.selected(); c != nil {
		tail = append(tail, p.statusLines(m, c, inner)...)
	}
	if p.inputMode == kubePickerInputNamespace {
		tail = append(tail, dialogLabel.Render("Namespace: ")+p.input.View())
	}
	if p.err != "" {
		tail = append(tail, wrapToWidth(kubeErrStyle.Render(kubeGlyphFailed+" "+p.err), inner))
	}
	if p.ns != nil {
		tail = append(tail, "", p.ns.view(m, inner))
	} else {
		tail = append(tail, dialogHint.Render(wrapToWidth(p.footer(m), inner)))
	}

	fixed := len(head) + strings.Count(strings.Join(tail, "\n"), "\n") + 1
	visible := kubeScroll(body, cursorStart, max(cursorEnd, cursorStart+1), kubeDialogBodyHeight(m, fixed), &p.offset)
	lines := append(append(append([]string(nil), head...), visible...), tail...)
	return kubeDialogFrame(m, strings.Join(lines, "\n"), maxWidth)
}

// statusLines describe the selected context: its namespace, the test in
// flight or its result, and what can be done next.
func (p *kubePicker) statusLines(m *model, c *fleetgrpc.KubeContext, inner int) []string {
	var out []string
	if p.repoint == "" {
		ns, suggested := p.namespaceFor(c)
		line := dimStyle.Render("namespace: ") + ns
		if suggested {
			line += dimStyle.Render("  (suggested: the context's is default · n changes it)")
		}
		out = append(out, line)
	}
	if p.busy != "" {
		return append(out, m.spinner.View()+" "+dimStyle.Render(p.busy))
	}
	run := p.run(m)
	if run == nil || !p.sameDraft(c) {
		return out
	}
	name := selectedStyle.Render("> " + kubeShortContext(c.GetName(), 24))
	switch {
	case run.running():
		out = append(out, kubeHangingIndent(name+"  "+m.spinner.View()+" "+dimStyle.Render("testing…")+" "+kubeCheckTrail(run.checks, 8), inner, 4))
	case run.err != nil:
		out = append(out, kubeHangingIndent(name+"  "+kubeErrStyle.Render(kubeGlyphFailed+" "+kubeTestError(run.err)), inner, 4))
	default:
		out = append(out, kubeHangingIndent(name+"  "+kubeBadge(run.result)+"  "+kubeCheckSummary(run.checks), inner, 4))
		for _, check := range run.checks {
			if check.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL {
				continue
			}
			line := "  " + check.GetDetail()
			if check.GetFix() != "" {
				line += dimStyle.Render(" · " + check.GetFix())
			}
			out = append(out, kubeHangingIndent(line, inner, 4))
		}
	}
	if !run.running() && p.failed {
		if kubeNamespaceMissing(run.checks) {
			out = append(out, "  "+selectedStyle.Render("[c] Create (shows labels)")+dimStyle.Render("  or [enter] connect anyway"))
		} else {
			out = append(out, dimStyle.Render("  [enter] connect anyway (Settings shows its health; fix and test it there)"))
		}
	}
	return out
}

// footer lists the keys that do something now.
func (p *kubePicker) footer(m *model) string {
	if p.inputMode != kubePickerInputNone {
		return "[enter] apply  [esc] cancel"
	}
	if p.filtering {
		return "type to filter  [↑/↓] move  [enter/esc] done"
	}
	c := p.selected()
	footer := kubeFooter(kubePickerKeys, func(k kubeKey) bool {
		switch {
		case kubeKeyIs(k, "L"):
			return c != nil && kubeLoginHereAvailable() && kubeUsesPlugin(c)
		case kubeKeyIs(k, "c"):
			return c != nil && p.namespaceMissing(m, c)
		case kubeKeyIs(k, "n"):
			return c != nil && p.repoint == ""
		}
		return true
	})
	if p.repoint != "" {
		footer = strings.Replace(footer, kubePickerConnect.hint, "[enter] re-point here", 1)
	}
	return footer
}
