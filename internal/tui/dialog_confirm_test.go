package tui

import (
	"errors"
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
		// The daemon lists the fleet until its job ends (the job's start
		// pushes the state): it stays off the list meanwhile.
		m.st = &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}
		fp.buildRows(m)
		if listsFleet(fp, "alpha") {
			t.Fatalf("%v: a state push put the fleet being removed back on the list", mode)
		}
		close(release)
		done, ok := cmd().(operationDoneMsg)
		if !ok || done.err != nil || !strings.Contains(done.message, "Removed fleet alpha") || !strings.Contains(done.message, "objects stay") {
			t.Fatalf("%v: result %+v", mode, done)
		}
	}
}

// The removal's end lets the fleet's name show again whichever page is open
// when it lands: a fleet the daemon still lists (a removal that failed), or a
// new one of the same name, is never kept off the list.
func TestARemovalsEndShowsTheFleetAgainFromAnyPage(t *testing.T) {
	origState := fetchStateLegacy
	fetchStateLegacy = func() (*state.State, error) { return nil, errors.New("daemon away") }
	t.Cleanup(func() { fetchStateLegacy = origState })
	fp := newFleetPage()
	fp.removingFleets = map[string]bool{"alpha": true}
	listed := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}
	m := model{currentPage: &settingsPage{}, fleetPage: fp, st: listed, width: 120, height: 40}
	fp.buildRows(&m)
	if listsFleet(fp, "alpha") {
		t.Fatal("a fleet being removed is listed")
	}
	next, _ := m.Update(operationDoneMsg{fleet: "alpha", err: errors.New("refused")})
	nm := next.(model)
	nm.st = listed
	nm.fleetPage.buildRows(&nm)
	if !listsFleet(nm.fleetPage, "alpha") {
		t.Fatal("a fleet the daemon lists after its removal ended is not shown")
	}
}

func listsFleet(fp *fleetPage, name string) bool {
	for _, r := range fp.rows {
		if r.kind == rowFleetHeader && r.fleetName == name {
			return true
		}
	}
	return false
}
