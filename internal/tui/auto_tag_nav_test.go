package tui

import (
	"runtime"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

func prStatusWithRefs(refs ...*fleetgrpc.PrRef) *fleetgrpc.PrStatus {
	return &fleetgrpc.PrStatus{
		OpenCount:    int32(len(refs)),
		PrSignal:     fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		Review:       fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
		ChecksPassed: 1,
		ChecksTotal:  1,
		ChecksSignal: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		Prs:          refs,
	}
}

func ref(n int32, url, title string) *fleetgrpc.PrRef {
	return &fleetgrpc.PrRef{Number: n, Url: url, Title: title}
}

// cursorOnInstance builds an expanded-instance model with the given PrStatus and
// parks the cursor on the instance row.
func cursorOnInstance(t *testing.T, inst *fleet.Instance, ps *fleetgrpc.PrStatus) (*fleetPage, *model) {
	t.Helper()
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.kind == rowInstance {
			fp.cursor = i
			return fp, m
		}
	}
	t.Fatalf("no instance row built")
	return nil, nil
}

func TestInstanceAutoTagSelectedIsBracketed(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	m := autoTagModel(newFleetPage(), inst, true, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	sel := m.instanceAutoTag("alpha", "agent-1", true)
	if !strings.Contains(sel, "[") || !strings.Contains(sel, "]") {
		t.Fatalf("selected auto tag should be bracketed: %q", sel)
	}
	if !strings.Contains(sel, "PR") || !strings.Contains(sel, "Checks 1/1") {
		t.Fatalf("selected auto tag missing content: %q", sel)
	}
	// Unselected has no brackets.
	if un := m.instanceAutoTag("alpha", "agent-1", false); strings.Contains(un, "[") {
		t.Fatalf("unselected auto tag should not be bracketed: %q", un)
	}
}

func TestAutoTagNavigable(t *testing.T) {
	running := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	withPR := autoTagModel(newFleetPage(), running, true, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	if !withPR.autoTagNavigable("alpha", running) {
		t.Errorf("instance with an open PR should be navigable")
	}

	noPR := autoTagModel(newFleetPage(), running, true, nil)
	if noPR.autoTagNavigable("alpha", running) {
		t.Errorf("instance with no PR status should not be navigable")
	}

	// A user-set tag takes the slot, so the auto tag is not navigable.
	tagged := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning, Tag: "wip"}
	withTag := autoTagModel(newFleetPage(), tagged, true, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	if withTag.autoTagNavigable("alpha", tagged) {
		t.Errorf("user-tagged instance should not be navigable")
	}
	if withTag.autoTagNavigable("alpha", nil) {
		t.Errorf("nil instance should not be navigable")
	}

	// Collapsed: the tag row isn't on screen, so it isn't navigable.
	collapsed := autoTagModel(newFleetPage(), running, false, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	if collapsed.autoTagNavigable("alpha", running) {
		t.Errorf("collapsed instance should not be navigable (tag row hidden)")
	}
}

func TestOpenSelectedPR_Single(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
	cmd := fp.openSelectedPR(m)
	if cmd == nil {
		t.Fatalf("expected a command to open the single PR")
	}
	if fp.mode != viewNormal {
		t.Errorf("single PR should not open the chooser; mode = %v", fp.mode)
	}
	if !strings.Contains(m.message, "42") {
		t.Errorf("message should mention PR number, got %q", m.message)
	}
}

func TestOpenSelectedPR_Multiple(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, prStatusWithRefs(
		ref(1, "https://x/pull/1", "a"), ref(2, "https://x/pull/2", "b")))
	if cmd := fp.openSelectedPR(m); cmd != nil {
		t.Errorf("multiple PRs should open a chooser (nil cmd), got non-nil")
	}
	if fp.mode != viewChoosePR {
		t.Fatalf("expected viewChoosePR, got %v", fp.mode)
	}
	if len(fp.choosePR.refs) != 2 {
		t.Errorf("chooser should hold 2 refs, got %d", len(fp.choosePR.refs))
	}
}

func TestOpenSelectedPR_None(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, nil)
	if cmd := fp.openSelectedPR(m); cmd != nil {
		t.Errorf("no PRs should yield no command")
	}
	if fp.mode != viewNormal {
		t.Errorf("mode should stay normal, got %v", fp.mode)
	}
}

func TestChoosePRDialogNavigation(t *testing.T) {
	fp := newFleetPage()
	m := &model{}
	fp.choosePR = choosePRState{refs: []*fleetgrpc.PrRef{
		ref(1, "u1", "a"), ref(2, "u2", "b"), ref(3, "u3", "c"),
	}}
	fp.mode = viewChoosePR

	key := func(s string) tea.KeyMsg {
		if s == "down" {
			return tea.KeyMsg{Type: tea.KeyDown}
		}
		if s == "up" {
			return tea.KeyMsg{Type: tea.KeyUp}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}

	fp.updateChoosePR(m, key("down"))
	if fp.choosePR.cursor != 1 {
		t.Fatalf("down -> cursor 1, got %d", fp.choosePR.cursor)
	}
	fp.updateChoosePR(m, key("up"))
	fp.updateChoosePR(m, key("up")) // wrap from 0 -> 2
	if fp.choosePR.cursor != 2 {
		t.Fatalf("up wrap -> cursor 2, got %d", fp.choosePR.cursor)
	}
	cmd := fp.updateChoosePR(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Errorf("enter should return an open command")
	}
	if fp.mode != viewNormal {
		t.Errorf("enter should close the dialog, mode = %v", fp.mode)
	}

	// esc closes without opening.
	fp.mode = viewChoosePR
	if cmd := fp.updateChoosePR(m, tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Errorf("esc should not open anything")
	}
	if fp.mode != viewNormal {
		t.Errorf("esc should close the dialog")
	}
}

func TestKeySelectDeselectAutoTag(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))

	runes := func(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

	fp.Update(m, runes('l')) // select
	if !fp.tagSelected {
		t.Fatalf("l should select the auto tag")
	}
	fp.Update(m, runes('h')) // deselect
	if fp.tagSelected {
		t.Fatalf("h should deselect the auto tag")
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRight}) // arrow select
	if !fp.tagSelected {
		t.Fatalf("right should select the auto tag")
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyLeft}) // arrow deselect
	if fp.tagSelected {
		t.Fatalf("left should deselect the auto tag")
	}
}

func TestKeySelectClearedByVerticalMove(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	fp.tagSelected = true
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}) // down
	if fp.tagSelected {
		t.Fatalf("vertical move should clear the tag selection")
	}
}

func TestKeyLOnNonNavigableInstanceDoesNotSelect(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, nil) // no PR status
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if fp.tagSelected {
		t.Fatalf("l on an instance without a navigable auto tag must not select")
	}
}

func TestBuildRowsClearsStaleSelection(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInstance(t, inst, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	fp.tagSelected = true
	// PR closes: clear the runtime PR status, rebuild — selection must drop.
	m.runtime[rtKey("alpha", "agent-1")] = &fleetgrpc.InstanceRuntime{Fleet: "alpha", Instance: "agent-1"}
	fp.buildRows(m)
	if fp.tagSelected {
		t.Fatalf("buildRows should clear selection once the auto tag is no longer navigable")
	}
}

func TestOpenExternalURLCommand(t *testing.T) {
	cmd, err := openExternalURLCommand("https://github.com/o/r/pull/7")
	if err != nil {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			t.Fatalf("unexpected error on %s: %v", runtime.GOOS, err)
		}
		return
	}
	found := false
	for _, a := range cmd.Args {
		if strings.Contains(a, "github.com/o/r/pull/7") {
			found = true
		}
	}
	if !found {
		t.Errorf("opener args should include the URL: %v", cmd.Args)
	}
}
