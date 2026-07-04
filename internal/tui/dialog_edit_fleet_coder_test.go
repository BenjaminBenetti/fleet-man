package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// openCoderTestDialog opens the edit-fleet dialog on fleet "alpha" (created
// with the given settings) and navigates to + expands the Coder section.
func openCoderTestDialog(t *testing.T, settings fleet.FleetSettings) (*fleetPage, *model, *fleet.Fleet) {
	t.Helper()
	f := &fleet.Fleet{Name: "alpha", Settings: settings}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
		spinner:   spinner.New(),
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if fp.mode != viewEditFleet {
		t.Fatalf("mode = %v, want viewEditFleet", fp.mode)
	}

	guard := 0
	for fp.dlg.row != editFleetRowCoder {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
		if guard++; guard > 30 {
			t.Fatal("never reached the Coder header")
		}
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) // expand
	if !fp.editFleet.coderExpanded {
		t.Fatal("Coder section should expand on l")
	}
	return fp, m, f
}

// TestEditFleetCoderSectionEditsAndSaves drives the Coder section end to end:
// workspace-name and parameter edits commit instantly, and the preset row
// cycles through the fetched preset list.
func TestEditFleetCoderSectionEditsAndSaves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{
		CoderTemplate:   "tmpl",
		CoderParameters: []fleet.CoderParameter{{Name: "cpus", DefaultValue: "2"}},
	})

	// Down onto the workspace-name row; typing enters the edit sub-mode.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.dlg.row != editFleetRowCoderWsName {
		t.Fatalf("dlg.row = %d, want coder workspace-name row", fp.dlg.row)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("acme")})
	if !fp.dlg.fieldActive {
		t.Fatal("typing on the workspace-name row should enter the edit sub-mode")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if f.Settings.CoderWorkspaceName != "acme" {
		t.Fatalf("CoderWorkspaceName = %q, want %q (instant-save)", f.Settings.CoderWorkspaceName, "acme")
	}

	// Down past the template row onto the preset row and cycle: with the
	// fetched preset list populated, l moves the selection and saves.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.dlg.row != editFleetRowCoderPreset {
		t.Fatalf("dlg.row = %d, want coder preset row", fp.dlg.row)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) // no presets: no-op
	if f.Settings.CoderPreset != "" {
		t.Fatalf("preset changed with no presets fetched: %q", f.Settings.CoderPreset)
	}
	fp.editFleet.coderPresets = []string{"small", "large"}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if f.Settings.CoderPreset != "large" {
		t.Fatalf("CoderPreset = %q, want %q (instant-save)", f.Settings.CoderPreset, "large")
	}

	// Down onto the parameter row; enter opens the shared input pre-loaded
	// with the current value, enter commits.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.dlg.row != editFleetRowCoderParamBase {
		t.Fatalf("dlg.row = %d, want first coder param row", fp.dlg.row)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !fp.dlg.fieldActive {
		t.Fatal("enter on a param row should open the editor")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("8")})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := f.Settings.CoderParameters[0].Value; got != "8" {
		t.Fatalf("param value = %q, want %q (instant-save)", got, "8")
	}
}

// TestEditFleetCoderTemplateCommitKicksFetch verifies committing a NEW
// template value saves it and fires the parameter fetch, whose result merges
// into the dialog (values kept by name, metadata refreshed, presets adopted).
func TestEditFleetCoderTemplateCommitKicksFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	origFetch := getCoderTemplateParamsRemote
	getCoderTemplateParamsRemote = func(template string) ([]coderRichParam, []string, error) {
		if template != "tmpl" {
			t.Fatalf("fetch called with template %q, want %q", template, "tmpl")
		}
		return []coderRichParam{
			{Name: "repo", DefaultValue: "d1", DisplayName: "Repo"},
			{Name: "cpus", DefaultValue: "2"},
		}, []string{"small", "large"}, nil
	}
	t.Cleanup(func() { getCoderTemplateParamsRemote = origFetch })

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{
		CoderParameters: []fleet.CoderParameter{{Name: "repo", Value: "keep-me"}},
	})

	// Down to the template row and type a new template.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.dlg.row != editFleetRowCoderTemplate {
		t.Fatalf("dlg.row = %d, want coder template row", fp.dlg.row)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tmpl")})
	cmd := fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if f.Settings.CoderTemplate != "tmpl" {
		t.Fatalf("CoderTemplate = %q, want %q (instant-save)", f.Settings.CoderTemplate, "tmpl")
	}
	if cmd == nil {
		t.Fatal("committing a new template should return the fetch cmd")
	}
	if !fp.editFleet.coderFetching {
		t.Fatal("coderFetching should be set while the fetch is in flight")
	}

	// Run the fetch cmd and feed its result back through the handler.
	msg, ok := cmd().(coderParamsFetchedMsg)
	if !ok {
		t.Fatal("fetch cmd did not return coderParamsFetchedMsg")
	}
	fp.handleCoderParamsFetched(m, msg)

	if fp.editFleet.coderFetching {
		t.Fatal("coderFetching should clear when the result lands")
	}
	params := f.Settings.CoderParameters
	if len(params) != 2 {
		t.Fatalf("want 2 merged params, got %d: %+v", len(params), params)
	}
	if params[0].Name != "repo" || params[0].Value != "keep-me" || params[0].DisplayName != "Repo" {
		t.Fatalf("user value / fetched metadata not merged: %+v", params[0])
	}
	if params[1].Name != "cpus" || params[1].Value != "" || params[1].DefaultValue != "2" {
		t.Fatalf("new param not adopted with default: %+v", params[1])
	}
	if f.Settings.CoderPreset != "small" {
		t.Fatalf("empty preset should default to the first fetched one, got %q", f.Settings.CoderPreset)
	}
	if len(fp.editFleet.coderPresets) != 2 {
		t.Fatalf("preset list not adopted: %v", fp.editFleet.coderPresets)
	}
}

// TestEditFleetCoderFetchIgnoresStaleFleet guards the stale-result path: a
// fetch that resolves after the dialog moved to a different fleet must not
// touch the open dialog's working state.
func TestEditFleetCoderFetchIgnoresStaleFleet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{CoderTemplate: "tmpl"})
	fp.editFleet.coderFetching = true

	fp.handleCoderParamsFetched(m, coderParamsFetchedMsg{
		fleetName: "beta",
		params:    []coderRichParam{{Name: "intruder"}},
		presets:   []string{"x"},
	})

	if !fp.editFleet.coderFetching {
		t.Fatal("a stale fleet's result must not clear this fleet's in-flight flag")
	}
	if len(f.Settings.CoderParameters) != 0 || len(fp.editFleet.coderParams) != 0 {
		t.Fatalf("stale result applied: %+v", fp.editFleet.coderParams)
	}
}

// TestEditFleetCoderVariablesHintScopedToParams guards the footer hint: the
// "${GIT_URL}" interpolation-variables hint shows on coder PARAMETER rows
// only, not on the template row above them.
func TestEditFleetCoderVariablesHintScopedToParams(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, _ := openCoderTestDialog(t, fleet.FleetSettings{
		CoderTemplate:   "tmpl",
		CoderParameters: []fleet.CoderParameter{{Name: "repo"}},
	})
	fp.editFleet.coderFetching = false // render without the spinner path

	fp.dlg.row = editFleetRowCoderTemplate
	if out := fp.renderEditFleet(m); strings.Contains(out, "${GIT_URL}") {
		t.Fatal("variables hint wrongly shown on the template row")
	}
	fp.dlg.row = editFleetRowCoderParamBase
	if out := fp.renderEditFleet(m); !strings.Contains(out, "${GIT_URL}") {
		t.Fatal("variables hint missing on a coder parameter row")
	}
}
