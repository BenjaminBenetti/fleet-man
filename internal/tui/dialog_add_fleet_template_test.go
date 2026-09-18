package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
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

// Before the first state snapshot m.st is nil; a fast n → file:// → Enter →
// Enter must not panic (the server's get-or-create still applies).
func TestAddFleetNameSubmitWithNilState(t *testing.T) {
	fp := newFleetPage()
	m := &model{fleetPage: fp}
	fp.mode = viewAddFleetName
	fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
	fp.textInput.SetValue("scratch")
	fp.dlg.fieldActive = true

	if cmd := fp.saveAddFleetName(m); cmd == nil || fp.mode != viewAddFleetInspecting {
		t.Fatalf("nil state: mode=%v cmd=%v, want inspecting with a cmd", fp.mode, cmd != nil)
	}
}

func TestAddFleetNameCancelClearsPending(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'q'}}, {Type: tea.KeyCtrlC}} {
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

// esc with the field inactive is "back": the URL step reopens with the
// template URL restored so a path typo can be fixed without starting over.
func TestAddFleetNameEscGoesBackToURLStep(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	// Longer than the name prompt's 64-rune CharLimit: the restore must not
	// truncate it (a cut path can name a DIFFERENT existing directory).
	longURL := "file:///home/benjamin/projects/experiments/scratch/very-long-project-name-v2"
	fp.mode = viewAddFleet
	fp.textInput.CharLimit = 256 // as the 'n' key sets it before typing
	fp.textInput.SetValue(longURL)
	fp.dlg.fieldActive = true
	fp.saveAddFleet(m) // → name prompt, CharLimit 64
	fp.dlg.fieldActive = false

	if view := fp.renderAddFleetNameDialog(m); !strings.Contains(view, "[esc] Back") {
		t.Fatalf("name prompt hint should advertise esc as Back:\n%s", view)
	}
	fp.updateAddFleetName(m, tea.KeyMsg{Type: tea.KeyEsc})

	if fp.mode != viewAddFleet {
		t.Fatalf("mode = %v, want viewAddFleet", fp.mode)
	}
	if got := fp.textInput.Value(); got != longURL {
		t.Fatalf("textInput = %q, want the full template URL restored (%d runes)", got, len([]rune(longURL)))
	}
	if !fp.dlg.fieldActive {
		t.Fatal("URL field should come back active so the cursor and hint agree")
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
	if !fp.dlg.fieldActive {
		t.Fatal("URL field should come back active after an inspect error")
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
// the backend list is computed, and open the dialog locked to devcontainer
// even when every backend CLI is installed.
func TestAddInstanceKeySetsTemplateFlagFromFleetRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origCheck := checkTools
	checkTools = allToolsFound
	t.Cleanup(func() { checkTools = origCheck })

	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "scratch"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"scratch": {Name: "scratch", Remote: "file:///home/me/scratch"}}},
		fleetPage: fp,
		config:    &configutil.Config{DefaultBackend: string(fleet.BackendCoder)},
	}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !fp.addInst.template {
		t.Fatal("addInst.template not set from the fleet's file:// remote")
	}
	if fp.mode != viewAddInstance {
		t.Fatalf("mode = %v, want viewAddInstance (message: %q)", fp.mode, m.message)
	}
	if fp.addInst.backend != fleet.BackendDevcontainer {
		t.Fatalf("backend = %v, want devcontainer for a template fleet even with coder as the configured default", fp.addInst.backend)
	}
}

// The Edit instance dialog must agree with the New instance dialog: a template
// instance's locked Branch row reads n/a, not "default".
func TestEditInstanceTemplateShowsBranchNA(t *testing.T) {
	fp := newFleetPage()
	f := &fleet.Fleet{Name: "scratch", Remote: "file:///home/me/scratch", Instances: []*fleet.Instance{{Name: "one"}}}
	fp.rows = []row{{kind: rowInstance, fleetName: "scratch", instance: f.Instances[0]}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"scratch": f}}, fleetPage: fp}

	fp.openEditInstanceDialog(m)

	if !fp.addInst.editing || !fp.addInst.template {
		t.Fatalf("editing=%v template=%v, want both true", fp.addInst.editing, fp.addInst.template)
	}
	view := fp.renderAddInstanceDialog(m)
	if !strings.Contains(view, templateBranchDisplay) {
		t.Fatalf("edit dialog should show the template n/a branch text:\n%s", view)
	}
	if strings.Contains(view, "default") {
		t.Fatalf("edit dialog must not show a 'default' branch for a template instance:\n%s", view)
	}

	// A recorded branch (an instance from before mixed-kind --repo was
	// rejected) is never hidden behind the n/a text.
	f.Instances[0].Branch = "main"
	fp.openEditInstanceDialog(m)
	view = fp.renderAddInstanceDialog(m)
	if !strings.Contains(view, "main") || strings.Contains(view, templateBranchDisplay) {
		t.Fatalf("edit dialog should show the recorded branch, not n/a:\n%s", view)
	}
}

func TestNoDevcontainerDialogCopyForTemplate(t *testing.T) {
	fp, m := newAddFleetModel(map[string]*fleet.Fleet{})
	fp.mode = viewAddFleetNoDevcontainer
	fp.addFleet.pendingRepoURL = "file:///home/me/scratch"
	view := fp.renderAddFleetNoDevcontainerDialog(m)
	if !strings.Contains(view, "This template dir has no") || strings.Contains(view, "clone the repo") {
		t.Fatalf("template no-devcontainer dialog should not talk about a repository/clone:\n%s", view)
	}
	fp.addFleet.pendingRepoURL = "git@github.com:org/repo.git"
	view = fp.renderAddFleetNoDevcontainerDialog(m)
	if !strings.Contains(view, "This repository has no") {
		t.Fatalf("git no-devcontainer dialog copy regressed:\n%s", view)
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
