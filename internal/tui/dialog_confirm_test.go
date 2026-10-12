package tui

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// Removing an empty fleet leaves the UI loop at once: the fleet goes from the
// list, and the destroy_fleet job (which waits for the fleet's caches and
// shares to go on every cluster) runs in the returned command, whose result
// carries the daemon's warnings.
func TestRemovingAnEmptyFleetRunsOffTheUILoop(t *testing.T) {
	for _, mode := range []viewMode{viewConfirmDelete, viewConfirmDeleteFleetWarn} {
		var calls atomic.Int32
		release := make(chan struct{})
		orig := destroyInstanceRemote
		destroyInstanceRemote = func(fleetName, instance string, destroyFleet bool) ([]string, error) {
			calls.Add(1)
			<-release // a cluster that takes its time
			if fleetName != "alpha" || instance != "" || !destroyFleet {
				t.Errorf("destroy %q/%q (fleet %v), want the whole fleet alpha", fleetName, instance, destroyFleet)
			}
			return []string{"mk-abc: the fleet's objects stay until the cluster can be reached"}, nil
		}
		t.Cleanup(func() { destroyInstanceRemote = orig })

		fp := newFleetPage()
		m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}, fleetPage: fp, width: 120, height: 40}
		fp.mode, fp.dlg.fleet = mode, "alpha"
		var cmd tea.Cmd
		if mode == viewConfirmDelete {
			cmd = fp.updateConfirmDelete(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
		} else {
			cmd = fp.updateConfirmDeleteFleetWarn(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
		}
		if cmd == nil || calls.Load() != 0 {
			t.Fatalf("%v: the removal ran in the key handler (calls %d, cmd %v)", mode, calls.Load(), cmd != nil)
		}
		if _, still := m.st.Fleets["alpha"]; still || fp.mode != viewNormal {
			t.Fatalf("%v: the fleet is still listed (mode %v)", mode, fp.mode)
		}
		close(release)
		done, ok := cmd().(operationDoneMsg)
		if !ok || done.err != nil || !strings.Contains(done.message, "Removed fleet alpha") || !strings.Contains(done.message, "objects stay") {
			t.Fatalf("%v: result %+v", mode, done)
		}
	}
}
