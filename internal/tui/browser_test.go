package tui

import (
	"os"
	"path/filepath"
	"testing"

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
  "customizations": { "fleet": {
    "browser": { "initialUrl": "https://example.com" },
    "fleetLaunch": { "sites": [ { "title": "API", "url": "http://localhost:3000" } ] }
  } }
}`

// stubBrowserConfig replaces the GetBrowserConfig RPC seam so the chooser-dialog
// tests don't need a running server. The actual devcontainer.json parsing lives
// in the server (internal/server/browser.go + the devcontainer package).
func stubBrowserConfig(t *testing.T, initialURL string, hasLanding bool) {
	t.Helper()
	orig := fetchBrowserConfig
	fetchBrowserConfig = func(string, string) (string, bool, error) { return initialURL, hasLanding, nil }
	t.Cleanup(func() { fetchBrowserConfig = orig })
}

// TestBeginBrowserOpenPromptsWhenUnset verifies the chooser dialog opens
// (rather than launching) when the fleet has no saved preference and the
// workspace configures both targets.
func TestBeginBrowserOpenPromptsWhenUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubBrowserConfig(t, "https://example.com", true)

	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, WorkspaceDir: t.TempDir()}
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
	stubBrowserConfig(t, "https://example.com", true)
	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, WorkspaceDir: t.TempDir()}
	f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{inst}}
	fp := newFleetPage()
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
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

// TestShouldSwitchBrowser documents the control-socket open decision matrix:
// a switch (kill + relaunch) is required only when a browser is actually
// running for the data dir and it is not already proxied to the requesting
// instance — with an unknown owner treated as "not this instance".
func TestShouldSwitchBrowser(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		active  string
		key     string
		want    bool
	}{
		{"none running", false, "", "fleet/b", false},
		{"none running, stale active", false, "fleet/a", "fleet/b", false},
		{"running, same instance", true, "fleet/b", "fleet/b", false},
		{"running, different instance", true, "fleet/a", "fleet/b", true},
		{"running, unknown owner", true, "", "fleet/b", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSwitchBrowser(tc.running, tc.active, tc.key); got != tc.want {
				t.Errorf("shouldSwitchBrowser(running=%v, active=%q, key=%q) = %v, want %v",
					tc.running, tc.active, tc.key, got, tc.want)
			}
		})
	}
}
