package tui

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// cursorOnFleetHeader builds a single-fleet model ("alpha") and parks the cursor
// on its header row, where the [automations]/[instances] mode toggle lives.
func cursorOnFleetHeader(t *testing.T) (*fleetPage, *model) {
	t.Helper()
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, false, nil)
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.kind == rowFleetHeader {
			fp.cursor = i
			return fp, m
		}
	}
	t.Fatalf("no fleet header row built")
	return nil, nil
}

func TestRightSelectFlipsAutomationMode(t *testing.T) {
	fp, m := cursorOnFleetHeader(t)

	if fp.automationMode["alpha"] {
		t.Fatal("fleet should start in instance mode")
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if !fp.rightSelected {
		t.Fatal("l should select the header's mode toggle")
	}

	// enter activates the toggle: flip to automation mode, keeping it focused.
	fp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !fp.automationMode["alpha"] {
		t.Fatal("enter on the selected toggle should switch to automation mode")
	}
	if !fp.rightSelected {
		t.Fatal("the toggle should stay focused after flipping so enter flips back")
	}
	if r := fp.currentRow(); r == nil || r.kind != rowFleetHeader {
		t.Fatalf("cursor should rest on the fleet header after toggling, got %#v", r)
	}

	// enter again flips back to the instance view.
	fp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if fp.automationMode["alpha"] {
		t.Fatal("a second enter should switch back to instance mode")
	}
}

func TestRightSelectHeaderDeselect(t *testing.T) {
	fp, m := cursorOnFleetHeader(t)
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	if !fp.rightSelected {
		t.Fatal("right should select the toggle")
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyLeft})
	if fp.rightSelected {
		t.Fatal("left should deselect the toggle")
	}
}

func TestRightSelectHeaderClearedByVerticalMove(t *testing.T) {
	fp, m := cursorOnFleetHeader(t)
	fp.rightSelected = true
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if fp.rightSelected {
		t.Fatal("a vertical move should clear the toggle selection")
	}
}

func TestSpaceFlipsModeWhenToggleSelected(t *testing.T) {
	// While the toggle is selected, space must flip the mode rather than
	// collapse/expand the fleet (the header's normal space action).
	fp, m := cursorOnFleetHeader(t)
	fp.rightSelected = true
	fp.Update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if !fp.automationMode["alpha"] {
		t.Fatal("space on the selected toggle should switch to automation mode")
	}
	if fp.collapsed["alpha"] {
		t.Fatal("space on the selected toggle must not collapse the fleet")
	}
}

func TestRightSelectNoOpWithoutRightElement(t *testing.T) {
	// On a plain instance row (no header toggle, no inline PR) →/l selects nothing.
	fp, m := cursorOnFleetHeader(t)
	for i, r := range fp.rows {
		if r.kind == rowInstance {
			fp.cursor = i
		}
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if fp.rightSelected {
		t.Fatal("l on a row with no right-hand element must not select")
	}
}
