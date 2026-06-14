package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// newFocusModel builds a two-fleet model ("alpha" and "beta", each with one
// running instance) with rows freshly built.
func newFocusModel() (*model, *fleetPage) {
	instA := &fleet.Instance{Name: "a1", Status: fleet.StatusRunning}
	instB := &fleet.Instance{Name: "b1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{instA}},
				"beta":  {Name: "beta", Instances: []*fleet.Instance{instB}},
			},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
		width:        100,
		height:       40,
	}
	fp.buildRows(m)
	return m, fp
}

func key(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func rowKinds(fp *fleetPage) []rowKind {
	kinds := make([]rowKind, len(fp.rows))
	for i, r := range fp.rows {
		kinds[i] = r.kind
	}
	return kinds
}

func fleetNamesInRows(fp *fleetPage) map[string]bool {
	names := map[string]bool{}
	for _, r := range fp.rows {
		if r.fleetName != "" {
			names[r.fleetName] = true
		}
	}
	return names
}

// ===========================================
// Entering / leaving
// ===========================================

func TestEnterFocusHidesOtherFleets(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")

	if fp.focusedFleet != "alpha" {
		t.Fatalf("focusedFleet = %q, want alpha", fp.focusedFleet)
	}
	names := fleetNamesInRows(fp)
	if !names["alpha"] || names["beta"] {
		t.Fatalf("focus rows should contain only alpha, got %v", names)
	}
	kinds := rowKinds(fp)
	if slices.Contains(kinds, rowSettings) {
		t.Fatalf("settings row must be hidden in focus mode, rows=%v", kinds)
	}
	if last := fp.rows[len(fp.rows)-1]; last.kind != rowLeaveFocus {
		t.Fatalf("last row kind = %v, want rowLeaveFocus", last.kind)
	}
	// Cursor parks on the focused fleet's header.
	if r := fp.currentRow(); r == nil || r.kind != rowFleetHeader || r.fleetName != "alpha" {
		t.Fatalf("cursor not on alpha header: %+v", r)
	}
}

func TestFocusKeyFromInstanceRowFocusesItsFleet(t *testing.T) {
	m, fp := newFocusModel()
	// Park the cursor on beta's instance row.
	for i, r := range fp.rows {
		if r.kind == rowInstance && r.fleetName == "beta" {
			fp.cursor = i
		}
	}
	if cmd := fp.updateNormal(m, key('f')); cmd != nil {
		t.Fatalf("focus key returned a command: %v", cmd)
	}
	if fp.focusedFleet != "beta" {
		t.Fatalf("focusedFleet = %q, want beta", fp.focusedFleet)
	}
	if fp.mode == viewPortForward {
		t.Fatalf("'f' must not open port-forward anymore")
	}
}

func TestFocusKeyOnSettingsRowDoesNothing(t *testing.T) {
	m, fp := newFocusModel()
	fp.cursor = len(fp.rows) - 1 // settings row
	if r := fp.currentRow(); r == nil || r.kind != rowSettings {
		t.Fatalf("expected cursor on settings, got %+v", r)
	}
	fp.updateNormal(m, key('f'))
	if fp.focusedFleet != "" {
		t.Fatalf("focus must not engage from the settings row, got %q", fp.focusedFleet)
	}
	if m.message == "" {
		t.Fatalf("expected a guidance message when focusing with no fleet selected")
	}
}

func TestFocusKeyTogglesOff(t *testing.T) {
	m, fp := newFocusModel()
	fp.updateNormal(m, key('f')) // enter on alpha (cursor starts on alpha header)
	if fp.focusedFleet == "" {
		t.Fatalf("focus did not engage")
	}
	fp.updateNormal(m, key('f')) // toggle off
	if fp.focusedFleet != "" {
		t.Fatalf("second 'f' should leave focus, got %q", fp.focusedFleet)
	}
}

func TestLeaveFocusRowEnterLeavesFocus(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")
	fp.cursor = len(fp.rows) - 1 // the leave-focus row
	if r := fp.currentRow(); r == nil || r.kind != rowLeaveFocus {
		t.Fatalf("expected cursor on rowLeaveFocus, got %+v", r)
	}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
	if fp.focusedFleet != "" {
		t.Fatalf("selecting [ leave focus ] should exit focus, got %q", fp.focusedFleet)
	}
	if slices.Contains(rowKinds(fp), rowLeaveFocus) {
		t.Fatalf("leave-focus row should be gone after leaving focus")
	}
}

func TestBuildRowsDropsFocusWhenFleetDeleted(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")
	delete(m.st.Fleets, "alpha")
	fp.buildRows(m)
	if fp.focusedFleet != "" {
		t.Fatalf("deleting the focused fleet should drop focus, got %q", fp.focusedFleet)
	}
	if !slices.Contains(rowKinds(fp), rowSettings) {
		t.Fatalf("settings row should return once focus is dropped")
	}
}

// ===========================================
// q / esc behave like a dialog in focus mode
// ===========================================

func TestQuitKeyLeavesFocusInsteadOfQuitting(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")
	cmd := fp.updateNormal(m, key('q'))
	if m.quitting {
		t.Fatalf("'q' in focus mode must not quit the app")
	}
	if cmd != nil {
		t.Fatalf("'q' in focus mode should not return a command")
	}
	if fp.focusedFleet != "" {
		t.Fatalf("'q' in focus mode should leave focus")
	}
}

func TestQuitKeyQuitsWhenNotFocused(t *testing.T) {
	m, fp := newFocusModel()
	cmd := fp.updateNormal(m, key('q'))
	if !m.quitting || cmd == nil {
		t.Fatalf("'q' outside focus mode should quit (quitting=%v cmd=%v)", m.quitting, cmd)
	}
}

func TestEscLeavesFocusButIsNoOpOtherwise(t *testing.T) {
	m, fp := newFocusModel()
	// esc with nothing focused: no quit, no change.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.quitting {
		t.Fatalf("esc outside focus mode must not quit")
	}

	fp.enterFocus(m, "alpha")
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEsc})
	if fp.focusedFleet != "" {
		t.Fatalf("esc in focus mode should leave focus")
	}
	if m.quitting {
		t.Fatalf("esc in focus mode must not quit")
	}
}

func TestArmadaFocusedQLeavesFocusAndClearsArmada(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")
	// 'k' from the focused header parks focus on the Armada selector.
	fp.updateNormal(m, key('k'))
	if !fp.armadaFocused {
		t.Fatalf("expected armada selector to take focus after 'k'")
	}
	cmd := fp.updateNormal(m, key('q'))
	if m.quitting || cmd != nil {
		t.Fatalf("'q' on the armada selector in focus mode must not quit")
	}
	if fp.focusedFleet != "" {
		t.Fatalf("'q' should have left focus mode")
	}
	if fp.armadaFocused {
		t.Fatalf("leaving focus must clear armadaFocused so the row cursor is visible again")
	}
}

func TestArmadaFocusedEscLeavesFocus(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")
	fp.updateNormal(m, key('k')) // focus the armada selector
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEsc})
	if fp.focusedFleet != "" {
		t.Fatalf("esc on the armada selector in focus mode should leave focus (same as q)")
	}
	if fp.armadaFocused {
		t.Fatalf("leaving focus must clear armadaFocused")
	}
}

func TestCtrlCStillQuitsInFocus(t *testing.T) {
	m, fp := newFocusModel()
	fp.enterFocus(m, "alpha")
	cmd := fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.quitting || cmd == nil {
		t.Fatalf("ctrl+c must always quit, even in focus mode")
	}
}

// ===========================================
// Port-forward moved to 'p'
// ===========================================

func TestPortForwardMovedToPKey(t *testing.T) {
	m, fp := newFocusModel()
	for i, r := range fp.rows {
		if r.kind == rowInstance && r.fleetName == "alpha" {
			fp.cursor = i
		}
	}
	fp.updateNormal(m, key('p'))
	if fp.mode != viewPortForward {
		t.Fatalf("'p' should open the port-forward dialog, mode=%v", fp.mode)
	}
}

// ===========================================
// View + help
// ===========================================

// tallBanner is a distinctive slice of the standard ASCII "fleet" logo, used to
// confirm the banner is unchanged by focus mode.
const tallBanner = "|_| |_"

func TestViewSwapsSettingsRowAndHidesHelpInFocus(t *testing.T) {
	m, fp := newFocusModel() // m.config == nil ⇒ help bar shown by default

	normal := fp.viewFleetList(m)
	if !strings.Contains(normal, "settings") {
		t.Fatalf("normal view should show the settings row")
	}
	if !strings.Contains(normal, "navigate") {
		t.Fatalf("normal view should show the help bar")
	}
	if !strings.Contains(normal, tallBanner) {
		t.Fatalf("normal view should show the standard banner")
	}

	fp.enterFocus(m, "alpha")
	focused := fp.viewFleetList(m)
	if !strings.Contains(focused, "[ leave focus ]") {
		t.Fatalf("focus view should show the leave-focus row:\n%s", focused)
	}
	if strings.Contains(focused, "settings") {
		t.Fatalf("focus view should not show the settings row:\n%s", focused)
	}
	// Focus mode hides the help bar...
	if strings.Contains(focused, "navigate") {
		t.Fatalf("focus view should hide the help bar:\n%s", focused)
	}
	// ...but leaves the banner exactly as it was (logo is unchanged).
	if !strings.Contains(focused, tallBanner) {
		t.Fatalf("focus view should keep the standard banner unchanged:\n%s", focused)
	}
}

func TestContextualHelpAdvertisesFocusAndMovedPortForward(t *testing.T) {
	m, fp := newFocusModel()

	// On a fleet header, not focused: "f: focus" offered.
	help := strings.Join(fp.contextualHelpKeys(m), " | ")
	if !strings.Contains(help, "f: focus") {
		t.Fatalf("fleet-row help should advertise 'f: focus': %s", help)
	}

	// Running instance row: port-forward advertised under 'p', not 'f'.
	for i, r := range fp.rows {
		if r.kind == rowInstance {
			fp.cursor = i
		}
	}
	ihelp := strings.Join(fp.contextualHelpKeys(m), " | ")
	if !strings.Contains(ihelp, "p: port-forward") || strings.Contains(ihelp, "f: port-forward") {
		t.Fatalf("instance help should move port-forward to 'p': %s", ihelp)
	}
}
