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
	// fetched preset list populated, l moves the selection and saves. The
	// cycle includes a leading "(none)" stop so "no preset" stays reachable.
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
	if f.Settings.CoderPreset != "small" {
		t.Fatalf("CoderPreset = %q, want %q (instant-save)", f.Settings.CoderPreset, "small")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if f.Settings.CoderPreset != "large" {
		t.Fatalf("CoderPreset = %q, want %q", f.Settings.CoderPreset, "large")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if f.Settings.CoderPreset != "" {
		t.Fatalf("CoderPreset = %q, want the (none) stop after wrapping", f.Settings.CoderPreset)
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
	if f.Settings.CoderPreset != "" {
		t.Fatalf("preset must NOT be auto-adopted on fetch (no-preset stays representable), got %q", f.Settings.CoderPreset)
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

// TestEditFleetCoderFetchIgnoresStaleTemplate guards the template-level
// staleness race: commit template a, then template b while fetch-a is in
// flight — fetch-a's late result must neither apply nor stop the spinner
// still waiting on fetch-b.
func TestEditFleetCoderFetchIgnoresStaleTemplate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{CoderTemplate: "b"})
	// Dialog open kicked fetch-b: coderFetchTemplate == "b", spinner on.
	if !fp.editFleet.coderFetching || fp.editFleet.coderFetchTemplate != "b" {
		t.Fatalf("open should kick fetch-b: fetching=%v template=%q", fp.editFleet.coderFetching, fp.editFleet.coderFetchTemplate)
	}

	fp.handleCoderParamsFetched(m, coderParamsFetchedMsg{
		fleetName: "alpha",
		template:  "a", // stale: answers an older commit
		params:    []coderRichParam{{Name: "intruder"}},
		presets:   []string{"x"},
	})

	if !fp.editFleet.coderFetching {
		t.Fatal("a stale template's result must not stop the newer fetch's spinner")
	}
	if len(f.Settings.CoderParameters) != 0 || len(fp.editFleet.coderPresets) != 0 {
		t.Fatalf("stale template result applied: %+v", fp.editFleet.coderParams)
	}
}

// TestEditFleetCoderClearedTemplateInvalidatesFetch guards the clear-template
// race: committing an empty template must invalidate the in-flight fetch so
// the removed template's parameters never land (and persist) on a fleet whose
// template is now empty, and the spinner must stop.
func TestEditFleetCoderClearedTemplateInvalidatesFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{CoderTemplate: "T"})
	if !fp.editFleet.coderFetching || fp.editFleet.coderFetchTemplate != "T" {
		t.Fatalf("open should kick fetch-T: fetching=%v template=%q", fp.editFleet.coderFetching, fp.editFleet.coderFetchTemplate)
	}

	// Clear the template and commit.
	fp.dlg.row = editFleetRowCoderTemplate
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	fp.coderTemplateInput.SetValue("")
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // commit
	if f.Settings.CoderTemplate != "" {
		t.Fatalf("CoderTemplate = %q, want cleared", f.Settings.CoderTemplate)
	}
	if fp.editFleet.coderFetching || fp.editFleet.coderFetchTemplate != "" {
		t.Fatalf("clearing must invalidate the fetch: fetching=%v template=%q", fp.editFleet.coderFetching, fp.editFleet.coderFetchTemplate)
	}

	// Fetch-T lands late: discarded.
	fp.handleCoderParamsFetched(m, coderParamsFetchedMsg{
		fleetName: "alpha",
		template:  "T",
		params:    []coderRichParam{{Name: "intruder"}},
	})
	if len(f.Settings.CoderParameters) != 0 || len(fp.editFleet.coderParams) != 0 {
		t.Fatalf("removed template's params applied: %+v", fp.editFleet.coderParams)
	}
}

// TestEditFleetCoderFetchStashedDuringWsNameEdit guards the broad mid-edit
// guard: the fetch-merge's persist snapshots every live text input, so a
// result landing while the workspace-name row is being edited would store the
// half-typed value. It must be deferred until the edit ends — then applied,
// not lost.
func TestEditFleetCoderFetchStashedDuringWsNameEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saves := 0
	orig := setFleetSettingsRemote
	setFleetSettingsRemote = func(string, fleet.FleetSettings) error { saves++; return nil }
	t.Cleanup(func() { setFleetSettingsRemote = orig })

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{CoderTemplate: "tmpl"})

	// Start typing a workspace name (half-typed, uncommitted).
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown}) // ws-name row
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dra")})
	if !fp.dlg.fieldActive {
		t.Fatal("ws-name edit should be active")
	}

	before := saves
	fp.handleCoderParamsFetched(m, coderParamsFetchedMsg{
		fleetName: "alpha",
		template:  "tmpl",
		params:    []coderRichParam{{Name: "repo"}}, // would change bindings -> persist
	})
	if saves != before {
		t.Fatalf("fetch persisted during an active ws-name edit (%d saves)", saves-before)
	}
	if f.Settings.CoderWorkspaceName != "" || len(f.Settings.CoderParameters) != 0 {
		t.Fatalf("half-typed edit persisted: %+v", f.Settings)
	}

	// Ending the edit commits the typed value AND applies the stashed fetch.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if f.Settings.CoderWorkspaceName != "dra" {
		t.Fatalf("CoderWorkspaceName = %q, want committed %q", f.Settings.CoderWorkspaceName, "dra")
	}
	if len(f.Settings.CoderParameters) != 1 || f.Settings.CoderParameters[0].Name != "repo" {
		t.Fatalf("stashed fetch not applied after the edit: %+v", f.Settings.CoderParameters)
	}
	if fp.editFleet.coderPendingFetch != nil {
		t.Fatal("pending fetch should be consumed")
	}
}

// TestEditFleetCoderClearingTemplateClearsBindings guards the clear-template
// semantics: "no template" must mean no template-scoped state — the create
// path passes --preset/--parameter regardless of --template, so a removed
// template's bindings left behind can hard-fail or mis-provision creation.
func TestEditFleetCoderClearingTemplateClearsBindings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{
		CoderTemplate:   "T",
		CoderPreset:     "large",
		CoderParameters: []fleet.CoderParameter{{Name: "repo", Value: "v"}},
	})
	fp.editFleet.coderPresets = []string{"small", "large"}

	fp.dlg.row = editFleetRowCoderTemplate
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	fp.coderTemplateInput.SetValue("")
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // commit the clear

	if f.Settings.CoderTemplate != "" || f.Settings.CoderPreset != "" || len(f.Settings.CoderParameters) != 0 {
		t.Fatalf("template-scoped state survived the clear: %+v", f.Settings)
	}
	if len(fp.editFleet.coderParams) != 0 || fp.editFleet.coderPreset != "" || len(fp.editFleet.coderPresets) != 0 {
		t.Fatalf("dialog working state survived the clear: params=%+v preset=%q presets=%v",
			fp.editFleet.coderParams, fp.editFleet.coderPreset, fp.editFleet.coderPresets)
	}
}

// TestEditFleetCoderFetchDeferredDuringParamEdit guards the mid-edit race: a
// fetch landing while a parameter edit is active must not reshape the list
// under the editor (the in-flight commit writes by row index); once the edit
// commits, the stashed result applies and the merge keeps the typed value.
func TestEditFleetCoderFetchDeferredDuringParamEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{
		CoderTemplate:   "tmpl",
		CoderParameters: []fleet.CoderParameter{{Name: "repo"}},
	})

	// Start editing the parameter row.
	fp.dlg.row = editFleetRowCoderParamBase
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !fp.dlg.fieldActive {
		t.Fatal("param edit should be active")
	}

	fp.handleCoderParamsFetched(m, coderParamsFetchedMsg{
		fleetName: "alpha",
		template:  "tmpl",
		params:    []coderRichParam{{Name: "repo"}, {Name: "cpus"}}, // would reshape the list
	})
	if got := fp.editFleet.coderParams; len(got) != 1 || got[0].Name != "repo" {
		t.Fatalf("fetch reshaped the param list under an active edit: %+v", got)
	}

	// The edit commits into the right parameter, then the stashed fetch
	// applies — merging by name, so the just-typed value survives.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	params := f.Settings.CoderParameters
	if len(params) != 2 || params[0].Name != "repo" || params[0].Value != "v" || params[1].Name != "cpus" {
		t.Fatalf("deferred merge wrong: %+v", params)
	}
}

// TestEditFleetCoderFetchUnchangedDoesNotPersist guards the dialog-open side
// effect: a fetch whose merge does not change the stored bindings must not
// rewrite the fleet's persisted settings.
func TestEditFleetCoderFetchUnchangedDoesNotPersist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saves := 0
	orig := setFleetSettingsRemote
	setFleetSettingsRemote = func(string, fleet.FleetSettings) error { saves++; return nil }
	t.Cleanup(func() { setFleetSettingsRemote = orig })

	param := fleet.CoderParameter{Name: "repo", Value: "v", DefaultValue: "d", DisplayName: "Repo"}
	fp, m, _ := openCoderTestDialog(t, fleet.FleetSettings{
		CoderTemplate:   "tmpl",
		CoderParameters: []fleet.CoderParameter{param},
	})

	before := saves
	fp.handleCoderParamsFetched(m, coderParamsFetchedMsg{
		fleetName: "alpha",
		template:  "tmpl",
		params:    []coderRichParam{{Name: "repo", DefaultValue: "d", DisplayName: "Repo"}},
		presets:   []string{"small"},
	})
	if saves != before {
		t.Fatalf("unchanged fetch persisted settings (%d saves)", saves-before)
	}
	if len(fp.editFleet.coderPresets) != 1 {
		t.Fatalf("preset list should still update in memory: %v", fp.editFleet.coderPresets)
	}
}

// TestEditFleetCoderWsNameValidatedLocally guards the client-side use of the
// shared domain rule: an illegal override is rejected with a local message
// (no RPC), and a legal mixed-case one is normalized to what `coder create`
// will receive.
func TestEditFleetCoderWsNameValidatedLocally(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saves := 0
	orig := setFleetSettingsRemote
	setFleetSettingsRemote = func(string, fleet.FleetSettings) error { saves++; return nil }
	t.Cleanup(func() { setFleetSettingsRemote = orig })

	fp, m, f := openCoderTestDialog(t, fleet.FleetSettings{})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown}) // workspace-name row

	// Illegal: rejected locally, nothing saved.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("bad name")})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if saves != 0 {
		t.Fatalf("invalid name reached the RPC (%d saves)", saves)
	}
	if !strings.Contains(m.message, "Invalid workspace name") {
		t.Fatalf("no local validation message: %q", m.message)
	}

	// Legal mixed case: normalized to lowercase and saved.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("MyProj")})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if f.Settings.CoderWorkspaceName != "myproj" {
		t.Fatalf("CoderWorkspaceName = %q, want normalized %q", f.Settings.CoderWorkspaceName, "myproj")
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
