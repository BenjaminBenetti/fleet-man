package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/editorlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// vscodeStubs stands in for the daemon, the editor and ~/.ssh/config.
type vscodeStubs struct {
	launch   editorlaunch.Launch
	err      error
	asked    []string
	ran      [][]string
	includes int
}

func stubVSCode(t *testing.T, s *vscodeStubs) {
	t.Helper()
	oldResolve, oldRun, oldAdd := resolveVSCode, runVSCode, addVSCodeInclude
	resolveVSCode = func(fleetName, instance string) (editorlaunch.Launch, error) {
		s.asked = append(s.asked, fleetName+"/"+instance)
		return s.launch, s.err
	}
	runVSCode = func(l editorlaunch.Launch) error {
		s.ran = append(s.ran, l.Argv)
		return nil
	}
	addVSCodeInclude = func() error {
		s.includes++
		s.launch.NeedsInclude = false
		return nil
	}
	t.Cleanup(func() { resolveVSCode, runVSCode, addVSCodeInclude = oldResolve, oldRun, oldAdd })
}

func vscodeModel() (*model, *fleetPage) {
	inst := &fleet.Instance{Name: "web", DisplayName: "Web", Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}}}},
		fleetPage: fp,
	}
	return m, fp
}

// pressVSCode runs a key and every command it leads to, feeding each message
// back to the page.
func pressVSCode(m *model, fp *fleetPage, key tea.KeyMsg) {
	var cmd tea.Cmd
	if fp.mode == viewNormal {
		cmd = fp.updateNormal(m, key)
	} else {
		cmd = fp.Update(m, key)
	}
	for cmd != nil {
		cmd = fp.Update(m, cmd())
	}
}

func TestCOpensWhatTheDaemonSays(t *testing.T) {
	s := &vscodeStubs{launch: editorlaunch.Launch{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_DEVCONTAINER_URI,
		Argv: []string{"code", "--folder-uri", "vscode-remote://dev-container+2f/workspaces/alpha"}}}
	stubVSCode(t, s)
	m, fp := vscodeModel()
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if !slices.Equal(s.asked, []string{"alpha/web"}) || len(s.ran) != 1 || !slices.Equal(s.ran[0], s.launch.Argv) {
		t.Fatalf("asked %q ran %q", s.asked, s.ran)
	}
	if fp.mode != viewNormal || m.message != "Opened VS Code for Web" {
		t.Fatalf("mode %v message %q", fp.mode, m.message)
	}

	s.err = errors.New("web is stopped")
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if len(s.ran) != 1 || !strings.Contains(m.message, "web is stopped") {
		t.Fatalf("an error ran the editor (%q) or was not shown: %q", s.ran, m.message)
	}
}

// Remote-SSH without the Include: the consent dialog first. Allow adds it
// and opens; manual steps and cancel change nothing.
func TestCAsksBeforeAddingTheInclude(t *testing.T) {
	remote := editorlaunch.Launch{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH, Alias: "web.alpha.fleet", NeedsInclude: true,
		Argv: []string{"code", "--folder-uri", "vscode-remote://ssh-remote+web.alpha.fleet/workspaces/alpha"}}
	s := &vscodeStubs{launch: remote}
	stubVSCode(t, s)
	m, fp := vscodeModel()

	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if fp.mode != viewEditorConsent || len(s.ran) != 0 {
		t.Fatalf("no consent dialog: mode %v ran %q", fp.mode, s.ran)
	}
	view := fp.renderEditorConsentDialog(m)
	for _, want := range []string{"Open in VS Code", "Include fleet_config", "[ Allow and open ]", "[ Show manual steps ]", "[ Cancel ]"} {
		if !strings.Contains(view, want) {
			t.Errorf("dialog lacks %q:\n%s", want, view)
		}
	}
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyEsc})
	if fp.mode != viewNormal || s.includes != 0 || len(s.ran) != 0 || !strings.Contains(m.message, "unchanged") {
		t.Fatalf("cancel: mode %v includes %d ran %q message %q", fp.mode, s.includes, s.ran, m.message)
	}

	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyEnter})
	if s.includes != 0 || len(s.ran) != 0 || !strings.Contains(m.message, "Include fleet_config") || !strings.Contains(m.message, "ssh-remote+web.alpha.fleet") {
		t.Fatalf("manual steps: includes %d ran %q message %q", s.includes, s.ran, m.message)
	}

	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyEnter})
	if s.includes != 1 || len(s.ran) != 1 || !slices.Equal(s.ran[0], remote.Argv) || fp.mode != viewNormal {
		t.Fatalf("allow: includes %d ran %q mode %v", s.includes, s.ran, fp.mode)
	}

	// With the Include there, no dialog.
	pressVSCode(m, fp, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if fp.mode != viewNormal || len(s.ran) != 2 || s.includes != 1 {
		t.Fatalf("second open: mode %v ran %d includes %d", fp.mode, len(s.ran), s.includes)
	}
}

// The daemon answers off the update loop: a dialog opened meanwhile stays,
// and the consent waits for the next c.
func TestConsentLeavesADialogOpenedMeanwhile(t *testing.T) {
	remote := editorlaunch.Launch{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH, Alias: "web.alpha.fleet", NeedsInclude: true,
		Argv: []string{"code", "--folder-uri", "vscode-remote://ssh-remote+web.alpha.fleet/workspaces/alpha"}}
	s := &vscodeStubs{launch: remote}
	stubVSCode(t, s)
	m, fp := vscodeModel()

	resolve := fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if resolve == nil {
		t.Fatal("c resolved nothing")
	}
	fp.mode = viewKubeRecovery
	if cmd := fp.Update(m, resolve()); cmd != nil {
		t.Fatal("the answer ran something")
	}
	if fp.mode != viewKubeRecovery || s.includes != 0 || len(s.ran) != 0 || !strings.Contains(m.message, "press c again") {
		t.Fatalf("mode %v includes %d ran %q message %q", fp.mode, s.includes, s.ran, m.message)
	}
}
