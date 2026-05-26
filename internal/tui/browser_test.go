package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// writeWorkspaceDevcontainer drops a devcontainer.json into
// <dir>/.devcontainer for the browser-target tests.
func writeWorkspaceDevcontainer(t *testing.T, dir, contents string) {
	t.Helper()
	cfgDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "devcontainer.json"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const bothTargetsDevcontainer = `{
  "customizations": { "fleet": { "browser": {
    "initialUrl": "https://example.com",
    "landingPage": { "sites": [ { "title": "API", "url": "http://localhost:3000" } ] }
  } } }
}`

// TestBothBrowserTargets verifies detection of "both initialUrl and a
// landing page are configured", plus the returned URL.
func TestBothBrowserTargets(t *testing.T) {
	t.Run("both", func(t *testing.T) {
		dir := t.TempDir()
		writeWorkspaceDevcontainer(t, dir, bothTargetsDevcontainer)
		url, both := bothBrowserTargets(dir)
		if !both || url != "https://example.com" {
			t.Fatalf("got (%q, %v), want (%q, true)", url, both, "https://example.com")
		}
	})
	t.Run("only url", func(t *testing.T) {
		dir := t.TempDir()
		writeWorkspaceDevcontainer(t, dir, `{"customizations":{"fleet":{"browser":{"initialUrl":"https://x"}}}}`)
		if _, both := bothBrowserTargets(dir); both {
			t.Fatalf("both = true, want false (only url)")
		}
	})
	t.Run("only landing", func(t *testing.T) {
		dir := t.TempDir()
		writeWorkspaceDevcontainer(t, dir, `{"customizations":{"fleet":{"browser":{"landingPage":{"sites":[{"title":"A","url":"u"}]}}}}}`)
		if _, both := bothBrowserTargets(dir); both {
			t.Fatalf("both = true, want false (only landing)")
		}
	})
	t.Run("missing config", func(t *testing.T) {
		if _, both := bothBrowserTargets(t.TempDir()); both {
			t.Fatalf("both = true, want false (no devcontainer.json)")
		}
	})
}

// TestBeginBrowserOpenPromptsWhenUnset verifies the chooser dialog opens
// (rather than launching) when the fleet has no saved preference and the
// workspace configures both targets.
func TestBeginBrowserOpenPromptsWhenUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeWorkspaceDevcontainer(t, dir, bothTargetsDevcontainer)

	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, WorkspaceDir: dir}
	f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{inst}}
	fp := newFleetPage()
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	if cmd := fp.beginBrowserOpen(m, inst, "alpha"); cmd != nil {
		t.Fatalf("expected nil cmd (dialog opened, no launch)")
	}
	if fp.mode != viewChooseBrowserLaunch {
		t.Fatalf("mode = %v, want viewChooseBrowserLaunch", fp.mode)
	}
}

// TestChooseBrowserLaunchPersists verifies the dialog choice writes the
// fleet's PreferFleetLaunch setting (true for Fleet Launch, false for URL)
// and closes the dialog.
func TestChooseBrowserLaunchPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, tc := range []struct {
		key  string
		want bool
	}{{"f", true}, {"u", false}} {
		dir := t.TempDir()
		writeWorkspaceDevcontainer(t, dir, bothTargetsDevcontainer)
		inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, WorkspaceDir: dir}
		f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{inst}}
		fp := newFleetPage()
		fp.mode = viewChooseBrowserLaunch
		fp.dialogFleet = "alpha"
		fp.dialogInst = "i1"
		m := &model{
			st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
			fleetPage: fp,
			backends:  map[fleet.BackendType]backend.Backend{},
		}

		fp.updateChooseBrowserLaunch(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})

		if !f.Settings.PreferFleetLaunchSet() {
			t.Fatalf("key %q: PreferFleetLaunch not set", tc.key)
		}
		if got := f.Settings.PreferFleetLaunchEnabled(); got != tc.want {
			t.Fatalf("key %q: PreferFleetLaunchEnabled = %v, want %v", tc.key, got, tc.want)
		}
		if fp.mode != viewNormal {
			t.Fatalf("key %q: mode = %v, want viewNormal", tc.key, fp.mode)
		}
	}
}

// TestChooseBrowserLaunchCursorEnter verifies cursor navigation + enter:
// the dialog opens on Fleet Launch, moving down to Initial URL and pressing
// enter saves false.
func TestChooseBrowserLaunchCursorEnter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeWorkspaceDevcontainer(t, dir, bothTargetsDevcontainer)
	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, WorkspaceDir: dir}
	f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{inst}}
	fp := newFleetPage()
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
		backends:  map[fleet.BackendType]backend.Backend{},
	}

	// Open the chooser; cursor defaults to Fleet Launch (row 0).
	fp.beginBrowserOpen(m, inst, "alpha")
	if fp.dialogRow != chooseBrowserRowFleetLaunch {
		t.Fatalf("default dialogRow = %d, want Fleet Launch", fp.dialogRow)
	}

	// Move down to Initial URL and choose it with enter.
	fp.updateChooseBrowserLaunch(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.dialogRow != chooseBrowserRowInitialURL {
		t.Fatalf("after down, dialogRow = %d, want Initial URL", fp.dialogRow)
	}
	fp.updateChooseBrowserLaunch(m, tea.KeyMsg{Type: tea.KeyEnter})

	if !f.Settings.PreferFleetLaunchSet() || f.Settings.PreferFleetLaunchEnabled() {
		t.Fatalf("enter on Initial URL: PreferFleetLaunch = %v, want set+false", f.Settings.PreferFleetLaunch)
	}
}

// TestShouldUseLandingPage covers the precedence between a configured
// browser.initialUrl and the Fleet Launch landing page, gated by the
// fleet's PreferFleetLaunch setting.
func TestShouldUseLandingPage(t *testing.T) {
	cases := []struct {
		name              string
		hasURL            bool
		hasLanding        bool
		preferFleetLaunch bool
		want              bool
	}{
		{"neither configured", false, false, false, false},
		{"neither, prefer on", false, false, true, false},
		{"only url", true, false, false, false},
		{"only url, prefer on", true, false, true, false},
		{"only landing", false, true, false, true},
		{"only landing, prefer on", false, true, true, true},
		{"both, prefer off -> url wins", true, true, false, false},
		{"both, prefer on -> landing wins", true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUseLandingPage(tc.hasURL, tc.hasLanding, tc.preferFleetLaunch); got != tc.want {
				t.Errorf("shouldUseLandingPage(url=%v, landing=%v, prefer=%v) = %v, want %v",
					tc.hasURL, tc.hasLanding, tc.preferFleetLaunch, got, tc.want)
			}
		})
	}
}
