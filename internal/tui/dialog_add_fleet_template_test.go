package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

func newAddFleetModel(fleets map[string]*fleet.Fleet) (*fleetPage, *model) {
	fp := newFleetPage()
	m := &model{st: &state.State{Fleets: fleets}, fleetPage: fp}
	return fp, m
}

// A file:// URL must not be inspected straight away: the dialog detours to
// the name prompt, pre-filled with the directory's base name.
func TestAddFleetTemplateURLPromptsForName(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleet
	fp.textInput.SetValue("file:///home/me/scratch-proj")
	fp.dlg.fieldActive = true

	cmd := fp.saveAddFleet(m)

	if fp.mode != viewAddFleetName {
		t.Fatalf("mode = %v, want viewAddFleetName", fp.mode)
	}
	if fp.addFleet.pendingRepoURL != "file:///home/me/scratch-proj" {
		t.Fatalf("pendingRepoURL = %q", fp.addFleet.pendingRepoURL)
	}
	if fp.addFleet.pendingFleetName != "" {
		t.Fatalf("fleet name must not be derived for a template, got %q", fp.addFleet.pendingFleetName)
	}
	if got := fp.textInput.Value(); got != "scratch-proj" {
		t.Fatalf("name prompt prefill = %q, want dir base name", got)
	}
	if !fp.dlg.fieldActive || cmd == nil {
		t.Fatal("name field should be active (blink cmd returned) so the user can type immediately")
	}
	view := fp.renderAddFleetNameDialog(m)
	for _, want := range []string{"Template:", "file:///home/me/scratch-proj", "Name:"} {
		if !strings.Contains(view, want) {
			t.Errorf("name dialog missing %q:\n%s", want, view)
		}
	}
}

// The URL step must advertise the file:// option.
func TestAddFleetDialogAdvertisesTemplateOption(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleet
	if view := fp.renderAddFleetDialog(m); !strings.Contains(view, "file:///abs/dir") {
		t.Fatalf("New fleet dialog does not mention file:// templates:\n%s", view)
	}
}

func TestAddFleetTemplateRelativePathStaysInDialog(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleet
	fp.textInput.SetValue("file://scratch")
	fp.dlg.fieldActive = true

	fp.saveAddFleet(m)

	if fp.mode != viewAddFleet {
		t.Fatalf("mode = %v, want to stay in viewAddFleet for correction", fp.mode)
	}
	if !strings.Contains(m.message, "absolute") {
		t.Fatalf("message = %q, want an absolute-path hint", m.message)
	}
}

func TestAddFleetNameSubmitStartsInspection(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleetName
	fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
	fp.textInput.SetValue("my-scratch")
	fp.dlg.fieldActive = true

	cmd := fp.updateAddFleetName(m, tea.KeyMsg{Type: tea.KeyEnter})

	if fp.mode != viewAddFleetInspecting {
		t.Fatalf("mode = %v, want viewAddFleetInspecting", fp.mode)
	}
	if fp.addFleet.pendingFleetName != "my-scratch" {
		t.Fatalf("pendingFleetName = %q", fp.addFleet.pendingFleetName)
	}
	if cmd == nil {
		t.Fatal("expected the inspect cmd to be returned")
	}
	if view := fp.renderAddFleetInspectingDialog(m); !strings.Contains(view, "Template:") {
		t.Fatalf("inspecting dialog should label a template source as Template:\n%s", view)
	}
}

func TestAddFleetNameRejectsInvalidOrExisting(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{"alpha": {Name: "alpha", Remote: "git@x:a.git"}})
	fp.mode = viewAddFleetName
	fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
	fp.dlg.fieldActive = true

	for _, name := range []string{"", "has space", "a/b", "alpha"} {
		fp.textInput.SetValue(name)
		fp.saveAddFleetName(m)
		if fp.mode != viewAddFleetName {
			t.Fatalf("name %q: mode = %v, want to stay on the prompt", name, fp.mode)
		}
		if m.message == "" {
			t.Fatalf("name %q: expected an error message", name)
		}
	}
	if !strings.Contains(m.message, "already exists") {
		t.Fatalf("existing fleet name should be refused, got %q", m.message)
	}
}

func TestAddFleetNameCancelClearsPending(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune{'q'}}, {Type: tea.KeyCtrlC}} {
		fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
		fp.mode = viewAddFleetName
		fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
		fp.dlg.fieldActive = key.Type == tea.KeyCtrlC

		fp.updateAddFleetName(m, key)

		if fp.mode != viewNormal {
			t.Fatalf("key %v: mode = %v, want viewNormal", key, fp.mode)
		}
		if fp.addFleet.pendingRepoURL != "" {
			t.Fatalf("key %v: pending URL not cleared", key)
		}
	}
}

// Inside the field, esc only leaves the field (matching the URL step).
func TestAddFleetNameEscLeavesFieldFirst(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleetName
	fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
	fp.dlg.fieldActive = true

	fp.updateAddFleetName(m, tea.KeyMsg{Type: tea.KeyEsc})

	if fp.mode != viewAddFleetName || fp.dlg.fieldActive {
		t.Fatalf("esc in field: mode=%v active=%v, want prompt kept with field inactive", fp.mode, fp.dlg.fieldActive)
	}
}

// After a failed inspection the URL step must show the URL again — the name
// prompt reused textInput for the fleet name.
func TestAddFleetInspectErrorRestoresTemplateURL(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleetInspecting
	fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
	fp.addFleet.pendingFleetName = "scratch"
	fp.textInput.SetValue("scratch")

	fp.handleDevcontainerInspected(m, devcontainerInspectedMsg{fleetName: "scratch", err: errString("template dir: no such file")})

	if fp.mode != viewAddFleet {
		t.Fatalf("mode = %v, want viewAddFleet", fp.mode)
	}
	if got := fp.textInput.Value(); got != "file:///home/me/scratch" {
		t.Fatalf("textInput = %q, want the template URL restored", got)
	}
	if !strings.Contains(m.message, "no such file") {
		t.Fatalf("message = %q", m.message)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A template fleet's New instance dialog has no branch to offer and only the
// devcontainer backend can consume a copied directory.
func TestAddInstanceTemplateFleetSkipsBranchAndLocksBackend(t *testing.T) {
	fp := newFleetPage()
	m := &model{
		st:         &state.State{Fleets: map[string]*fleet.Fleet{"scratch": {Name: "scratch", Remote: "file:///home/me/scratch"}}},
		fleetPage:  fp,
		toolStatus: allToolsFound(),
		creating:   map[string]bool{},
	}
	fp.mode = viewAddInstance
	fp.dlg.fleet = "scratch"
	fp.addInst.template = true
	fp.addInst.backend = fleet.BackendDevcontainer
	fp.addInst.color = instanceColorWhite

	if got := fp.availableBackendTypes(m); len(got) != 1 || got[0] != fleet.BackendDevcontainer {
		t.Fatalf("availableBackendTypes = %v, want [devcontainer]", got)
	}
	if fp.addInstanceRowEnabled(addInstanceRowBranch) {
		t.Fatal("branch row must be disabled for a template fleet")
	}
	if next := fp.nextAddInstanceRow(addInstanceRowName); next != addInstanceRowColor {
		t.Fatalf("down from Name landed on row %d, want Color (branch skipped)", next)
	}
	if prev := fp.prevAddInstanceRow(addInstanceRowColor); prev != addInstanceRowName {
		t.Fatalf("up from Color landed on row %d, want Name (branch skipped)", prev)
	}
	if view := fp.renderAddInstanceDialog(m); !strings.Contains(view, "n/a") {
		t.Fatalf("branch row should read n/a for a template fleet:\n%s", view)
	}

	// A stale branch value must not leak into the create request.
	var gotBranch, gotRemote string
	orig := createInstanceRemote
	createInstanceRemote = func(fleetName, instanceName, remote, branch string, backendType fleet.BackendType) error {
		gotRemote, gotBranch = remote, branch
		return nil
	}
	t.Cleanup(func() { createInstanceRemote = orig })
	fp.textInput.SetValue("agent-1")
	fp.branchInput.SetValue("stale")
	cmd := fp.submitAddInstance(m)
	if cmd == nil {
		t.Fatal("submit should return the create cmd")
	}
	cmd()
	if gotBranch != "" {
		t.Fatalf("branch %q sent for a template fleet, want empty", gotBranch)
	}
	if gotRemote != "file:///home/me/scratch" {
		t.Fatalf("remote = %q", gotRemote)
	}
}

// Pressing 'a' on a template fleet header must arm the template flag before
// the backend list is computed.
func TestAddInstanceKeySetsTemplateFlagFromFleetRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "scratch"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"scratch": {Name: "scratch", Remote: "file:///home/me/scratch"}}},
		fleetPage: fp,
	}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !fp.addInst.template {
		t.Fatal("addInst.template not set from the fleet's file:// remote")
	}
	if fp.mode == viewAddInstance && fp.addInst.backend != fleet.BackendDevcontainer {
		t.Fatalf("backend = %v, want devcontainer for a template fleet", fp.addInst.backend)
	}
}

func TestFirstFleetRepoSkipsTemplateFleets(t *testing.T) {
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{
		"scratch": {Name: "scratch", Remote: "file:///home/me/scratch"},
	}}}
	if got := m.firstFleetRepo(); got != "" {
		t.Fatalf("firstFleetRepo = %q for a template-only state, want empty", got)
	}
	m.st.Fleets["real"] = &fleet.Fleet{Name: "real", Remote: "git@github.com:org/real.git"}
	if got := m.firstFleetRepo(); got != "org/real" {
		t.Fatalf("firstFleetRepo = %q, want org/real", got)
	}
}
