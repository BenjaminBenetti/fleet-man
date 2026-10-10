package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
)

// kubeCandidateKeys is every key the keys test tries on each view: letters,
// digits, punctuation, the named keys and the ctrl chords (but ctrl+c and
// ctrl+q, the Global quit keys).
func kubeCandidateKeys() []string {
	var out []string
	for r := 'a'; r <= 'z'; r++ {
		out = append(out, string(r), strings.ToUpper(string(r)))
		if r != 'c' && r != 'q' {
			out = append(out, "ctrl+"+string(r))
		}
	}
	for r := '0'; r <= '9'; r++ {
		out = append(out, string(r))
	}
	for _, r := range "/.,-=?[];:'!@#*+" {
		out = append(out, string(r))
	}
	return append(out, "enter", "esc", "tab", "shift+tab", "up", "down", "left", "right", " ", "backspace")
}

// kubeKeyCovered reports whether a table documents key (as tea names it).
func kubeKeyCovered(tables [][]kubeKey, key string) bool {
	for _, table := range tables {
		for _, k := range table {
			if kubeKeyIs(k, key) {
				return true
			}
		}
	}
	return false
}

// checkKubeKeys feeds every candidate key to a fresh view (setup) and fails
// for each key the view acts on (returns a command, or changes what
// fingerprint reads) that tables do not document.
func checkKubeKeys(t *testing.T, view string, tables [][]kubeKey, setup func(t *testing.T) (send func(tea.KeyMsg) tea.Cmd, fingerprint func() string)) {
	t.Helper()
	acted := 0
	for _, s := range kubeCandidateKeys() {
		key := kubeKeyMsg(s)
		send, fingerprint := setup(t)
		before := fingerprint()
		cmd := send(key)
		if cmd == nil && fingerprint() == before {
			continue
		}
		acted++
		if !kubeKeyCovered(tables, key.String()) {
			t.Errorf("%s acts on %q, which neither its footer nor the keybindings list names", view, key.String())
		}
	}
	if acted == 0 {
		t.Fatalf("%s acted on no key: the probe is broken", view)
	}
}

// stubLoginHere keeps [L] from reading a real kubeconfig.
func stubLoginHere(t *testing.T) {
	orig := kubeLoginHereCommandFn
	kubeLoginHereCommandFn = func(*fleetgrpc.KubeCluster) (*exec.Cmd, error) { return nil, errors.New("stub") }
	t.Cleanup(func() { kubeLoginHereCommandFn = orig })
}

// Every key a Kubernetes view acts on is in its key table, which is what
// the footer and the keybindings list are built from.
func TestKubeKeysAreDocumented(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stubLoginHere(t)
	// Settings' own navigation (j/k, esc/q) is in the Settings group.
	settingsKeys := []kubeKey{{keys: []string{"j", "k", "up", "down", "esc", "q"}}}

	for _, focus := range []int{0, 1, 2} {
		checkKubeKeys(t, fmt.Sprintf("a cluster row (button %d)", focus), [][]kubeKey{kubeRowKeys, settingsKeys}, func(t *testing.T) (func(tea.KeyMsg) tea.Cmd, func() string) {
			m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2), kubeSt("eu-1", "prod", 2))
			m.fleetPage = newFleetPage()
			sp.cursor = settingsPositionOf(sp, m, settingsItemKubeBase)
			sp.kubeTestFocused, sp.kubeDeleteFocused = focus == 1, focus == 2
			return func(k tea.KeyMsg) tea.Cmd { return sp.Update(m, k) }, func() string {
				return fmt.Sprint(sp.cursor, sp.kubeTestFocused, sp.kubeDeleteFocused, sp.kubeDialog != nil, m.currentPage == sp)
			}
		})
	}

	checkKubeKeys(t, "the context picker", [][]kubeKey{kubePickerKeys}, func(t *testing.T) (func(tea.KeyMsg) tea.Cmd, func() string) {
		m, sp := kubeTestModel(t)
		p := newKubePicker(m, "")
		sp.kubeDialog = p
		p.onContexts(m, kubeContextsMsg{reply: pickerContexts()})
		p.runKey, p.testCtx, p.testNS, p.failed = "draft:x", "minikube", "fleet", true
		m.kubeRuns = map[string]*kubeTestRun{"draft:x": {key: "draft:x", done: true, cancel: func() {},
			checks: []*fleetgrpc.KubeCheck{{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL}}}}
		return func(k tea.KeyMsg) tea.Cmd { return sp.Update(m, k) }, func() string {
			return fmt.Sprint(p.cursor, p.filtering, p.filter.Value(), p.inputMode, p.runKey, p.busy, p.ns != nil, sp.kubeDialog == kubeDialog(p), p.err)
		}
	})

	for _, path := range []string{"display_name", "namespace", "clone_method", "allow_privileged", "", "group"} {
		checkKubeKeys(t, "the cluster editor on "+path, [][]kubeKey{kubeEditorKeys}, func(t *testing.T) (func(tea.KeyMsg) tea.Cmd, func() string) {
			st := kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED)
			st.Checks = []*fleetgrpc.KubeCheck{{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL}}
			m, sp := kubeTestModel(t, st)
			e := newKubeEditor("mk-1")
			sp.kubeDialog = e
			switch path {
			case "":
				e.row = len(kubeTopFields) // Status
			case "group":
				e.row = len(kubeTopFields) + 1
			default:
				editorRowOf(t, e, path)
			}
			return func(k tea.KeyMsg) tea.Cmd { return sp.Update(m, k) }, func() string {
				return fmt.Sprint(e.row, e.button, e.editing != nil, e.open, e.ns != nil, len(e.saving), sp.kubeDialog == kubeDialog(e), e.fieldErr)
			}
		})
	}

	checkKubeKeys(t, "the namespace panel", [][]kubeKey{kubeNamespaceKeys}, func(t *testing.T) (func(tea.KeyMsg) tea.Cmd, func() string) {
		m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
		e := newKubeEditor("mk-1")
		sp.kubeDialog = e
		e.ns = &kubeNamespacePanel{id: "mk-1", name: "fleet", manifest: "kind: Namespace\n"}
		return func(k tea.KeyMsg) tea.Cmd { return sp.Update(m, k) }, func() string {
			return fmt.Sprint(e.ns != nil && e.ns.creating, e.ns == nil, m.message)
		}
	})

	checkKubeKeys(t, "the disconnect dialog", [][]kubeKey{kubeRemoveKeys}, func(t *testing.T) (func(tea.KeyMsg) tea.Cmd, func() string) {
		m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
		d := newKubeRemoveDialog(m, "mk-1")
		d.instances = []kubeUser{{fleet: "fm", instance: "a"}}
		sp.kubeDialog = d
		return func(k tea.KeyMsg) tea.Cmd { return sp.Update(m, k) }, func() string {
			return fmt.Sprint(d.cursor, d.typing, d.busy, d.err, sp.kubeDialog == kubeDialog(d))
		}
	})
}

// Every key table entry is in the keybindings list's Kubernetes group and in
// its view's footer (in a state where the key does something).
func TestKubeKeysInKeybindingsAndFooters(t *testing.T) {
	stubLoginHere(t)
	var group *keybindingGroup
	for _, g := range keybindingsData() {
		if g.Title == "Kubernetes (Settings)" {
			group = &g
		}
	}
	if group == nil {
		t.Fatal("keybindingsData has no Kubernetes group")
	}
	listed := map[string]bool{}
	for _, e := range group.Entries {
		listed[e.Key+"\x00"+e.Desc] = true
	}
	dialogs := map[string]bool{}
	for _, g := range keybindingsData() {
		if g.Title == "Dialogs" {
			for _, e := range g.Entries {
				dialogs[e.Key] = true
			}
		}
	}
	for _, table := range kubeKeyTables() {
		for _, k := range table {
			switch {
			case k.shared && !dialogs[k.label]:
				t.Errorf("shared key %q is not in the Dialogs group", k.label)
			case !k.shared && !listed[k.label+"\x00"+k.desc]:
				t.Errorf("keybindings list lacks %q %q", k.label, k.desc)
			}
		}
	}

	footerHas := func(view, footer string, table []kubeKey) {
		for _, k := range table {
			if !strings.Contains(footer, k.hint) {
				t.Errorf("%s footer %q lacks %q", view, footer, k.hint)
			}
		}
	}

	m, sp := kubeTestModel(t, kubeSt("mk-1", "minikube", 2))
	footerHas("cluster row", sp.kubeRowFooter(settingsItemKubeBase), kubeRowKeys)

	p := newKubePicker(m, "")
	sp.kubeDialog = p
	p.onContexts(m, kubeContextsMsg{reply: pickerContexts()})
	p.runKey, p.testCtx, p.testNS, p.failed = "draft:x", "minikube", "fleet", true
	m.kubeRuns = map[string]*kubeTestRun{"draft:x": {key: "draft:x", done: true, cancel: func() {},
		checks: []*fleetgrpc.KubeCheck{{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL}}}}
	footerHas("picker", p.footer(m), kubePickerKeys)
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, strings.Join(strings.Fields(p.footer(m)), " ")) {
		t.Errorf("the picker must render its footer:\n%s", out)
	}

	st := kubeSt("mk-1", "minikube", 9)
	st.Checks = []*fleetgrpc.KubeCheck{
		{Name: "credentials", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: "exec kubelogin (expires in 54m)"},
		{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL},
	}
	m.kubeClusters.Clusters = []*fleetgrpc.KubeClusterStatus{st}
	e := newKubeEditor("mk-1")
	editorRowOf(t, e, "clone_method")
	footerHas("editor", e.footer(m, st), kubeEditorKeys)
	footerHas("namespace panel", (&kubeNamespacePanel{name: "fleet"}).view(m, 80), kubeNamespaceKeys)
	footerHas("disconnect dialog", flatText(newKubeRemoveDialog(m, "mk-1").view(m, sp)), kubeRemoveKeys)
}
