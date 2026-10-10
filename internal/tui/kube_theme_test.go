package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/theme"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// kubeThemeStatuses is one connection in each state, with warnings and
// checks, so every badge glyph shows.
func kubeThemeStatuses() []*fleetgrpc.KubeClusterStatus {
	var out []*fleetgrpc.KubeClusterStatus
	for state := range fleetgrpc.KubeClusterState_name {
		st := kubeSt(fleetgrpc.KubeClusterState(state).String(), "c"+fleetgrpc.KubeClusterState(state).String()[19:], fleetgrpc.KubeClusterState(state))
		st.Message = "a reason"
		st.Warnings = []string{"privileged allowed: the node is the isolation boundary"}
		st.Checks = []*fleetgrpc.KubeCheck{
			{Name: "rbac", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS},
			{Name: "admission_debug_caps", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN},
			{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL},
		}
		out = append(out, st)
	}
	return out
}

// kubeGlyphSlots is the theme slot each badge glyph must be drawn in.
func kubeGlyphSlots(t theme.Theme) map[string]lipgloss.Color {
	return map[string]lipgloss.Color{
		kubeGlyphReady: t.Success, kubeGlyphPass: t.Success,
		kubeGlyphLimited: t.Warning, kubeGlyphWarn: t.Warning,
		kubeGlyphFailed: t.Error,
	}
}

// sgrBefore is the style sequence lipgloss writes in front of text in color.
func sgrBefore(color lipgloss.Color) string {
	out := lipgloss.NewStyle().Foreground(color).Render("x")
	return out[:strings.Index(out, "x")]
}

// assertGlyphSlots fails for a badge glyph in out not drawn in its slot.
func assertGlyphSlots(t *testing.T, view string, out string, th theme.Theme) {
	t.Helper()
	found := 0
	for glyph, slot := range kubeGlyphSlots(th) {
		want := sgrBefore(slot)
		for rest, at := out, 0; ; {
			i := strings.Index(rest, glyph)
			if i < 0 {
				break
			}
			found++
			pos := at + i
			if !strings.HasSuffix(out[:pos], want) {
				t.Errorf("%s under %s: %q at byte %d is not drawn in its slot (%q); preceded by %q", view, th.Name, glyph, pos, want, out[max(0, pos-24):pos])
			}
			rest, at = rest[i+len(glyph):], pos+len(glyph)
		}
	}
	if found == 0 {
		t.Fatalf("%s under %s: no badge glyph rendered", view, th.Name)
	}
}

// Settings → Kubernetes, the context picker, the cluster editor and the
// disconnect dialog draw every badge (●, ✓, ▲, ⚠, ✕) in its theme slot under
// all 8 themes, so the light themes keep them readable.
func TestKubeBadgesUseThemeSlotsInEveryTheme(t *testing.T) {
	resetTheme(t)
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	if len(theme.All()) != 8 {
		t.Fatalf("%d themes: update this test's premise", len(theme.All()))
	}
	for _, th := range theme.All() {
		applyTheme(th)
		m, sp := kubeTestModel(t, kubeThemeStatuses()...)
		m.st.Fleets["fm"] = &fleet.Fleet{Name: "fm", Instances: []*fleet.Instance{{Name: "a", KubeCluster: m.kubeStatuses()[0].GetCluster().GetId()}}}

		view := sp.viewSettings(m)
		start, end := strings.Index(view, "Kubernetes"), strings.Index(view, "Browser")
		if start < 0 || end < start {
			t.Fatal("no Kubernetes section")
		}
		assertGlyphSlots(t, "Settings → Kubernetes", view[start:end], th)

		p := newKubePicker(m, "")
		p.onContexts(m, kubeContextsMsg{reply: pickerContexts()})
		p.runKey, p.testCtx, p.testNS = "draft:x", "minikube", "fleet"
		result := kubeSt("", "", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED)
		m.kubeRuns = map[string]*kubeTestRun{"draft:x": {key: "draft:x", done: true, cancel: func() {}, result: result, checks: kubeThemeStatuses()[0].Checks}}
		sp.kubeDialog = p
		assertGlyphSlots(t, "the context picker", sp.viewSettings(m), th)

		for _, st := range m.kubeStatuses() {
			e := newKubeEditor(st.GetCluster().GetId())
			for _, g := range kubeGroups {
				e.open[g.key] = true
			}
			sp.kubeDialog = e
			assertGlyphSlots(t, "the cluster editor ("+st.GetState().String()+")", sp.viewSettings(m), th)
		}

		d := newKubeRemoveDialog(m, m.kubeStatuses()[0].GetCluster().GetId())
		d.err = "refused"
		sp.kubeDialog = d
		assertGlyphSlots(t, "the disconnect dialog", sp.viewSettings(m), th)
	}
}

// The TUI draws no color it did not take from a theme slot: no color
// literals (lipgloss.Color("..."), hex strings, ANSI color escapes) in its
// non-test sources. The banner gradient's computed colors and the tmux status
// format fleet writes inside instance sessions (pane content, which themes
// leave alone) are not literals of this kind.
func TestTUISourcesHaveNoColorLiterals(t *testing.T) {
	literal := regexp.MustCompile(`lipgloss\.Color\(\s*"|"#[0-9a-fA-F]{3}(?:[0-9a-fA-F]{3})?"|\\x1b\[[0-9;]*(?:3[0-9]|4[0-9]|9[0-7]|10[0-7])(?:;[0-9;]*)?m|\\033\[[0-9;]*3[0-9]`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if loc := literal.FindStringIndex(line); loc != nil {
				t.Errorf("%s:%d: a color literal (%q): draw it from a theme slot (internal/theme)", f, i+1, line[loc[0]:loc[1]])
			}
		}
	}
	if checked < 50 {
		t.Fatalf("checked only %d files: wrong directory?", checked)
	}
}
