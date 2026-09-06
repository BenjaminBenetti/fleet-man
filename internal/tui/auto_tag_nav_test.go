package tui

import (
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

// cursorOnInlinePR builds an expanded-instance model with the given PrStatus and
// parks the cursor on the child row that carries the inline PR status.
func cursorOnInlinePR(t *testing.T, inst *fleet.Instance, ps *fleetgrpc.PrStatus) (*fleetPage, *model) {
	t.Helper()
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.prStatusInline {
			fp.cursor = i
			return fp, m
		}
	}
	t.Fatalf("no inline-PR row built")
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
	if un := m.instanceAutoTag("alpha", "agent-1", false); strings.Contains(un, "[") {
		t.Fatalf("unselected auto tag should not be bracketed: %q", un)
	}
}

func TestRowInlinePRRefs(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	m := autoTagModel(newFleetPage(), inst, true, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))

	inlineRow := row{kind: rowNewSession, fleetName: "alpha", instance: inst, prStatusInline: true}
	if got := m.rowInlinePRRefs(inlineRow); len(got) != 1 {
		t.Errorf("inline row should expose 1 PR ref, got %d", len(got))
	}
	// Same row without the inline flag carries nothing.
	plainRow := row{kind: rowNewSession, fleetName: "alpha", instance: inst}
	if got := m.rowInlinePRRefs(plainRow); got != nil {
		t.Errorf("non-inline row should expose no refs, got %d", len(got))
	}
	// Inline flag but no PR status -> nothing.
	noPR := autoTagModel(newFleetPage(), inst, true, nil)
	if got := noPR.rowInlinePRRefs(inlineRow); got != nil {
		t.Errorf("inline row with no PR status should expose no refs")
	}
}

func TestSelectedInlinePR(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(ref(7, "https://x/pull/7", "t")))
	if len(fp.selectedInlinePR(m)) != 1 {
		t.Errorf("cursor on the inline-PR row should expose its refs")
	}
	// Move the cursor up to the instance row: no inline PR there.
	fp.cursor--
	if fp.rows[fp.cursor].kind != rowInstance {
		t.Fatalf("expected the instance row above the inline-PR row")
	}
	if fp.selectedInlinePR(m) != nil {
		t.Errorf("instance row carries no inline PR status")
	}
}

func TestOpenSelectedPR_Single(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
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
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(
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
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, nil)
	fp.buildRows(m)
	// Park on the "+ new session" row (no inline PR since there's no PR status).
	for i, r := range fp.rows {
		if r.kind == rowNewSession {
			fp.cursor = i
		}
	}
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
		switch s {
		case "down":
			return tea.KeyMsg{Type: tea.KeyDown}
		case "up":
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

	fp.mode = viewChoosePR
	if cmd := fp.updateChoosePR(m, tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Errorf("esc should not open anything")
	}
	if fp.mode != viewNormal {
		t.Errorf("esc should close the dialog")
	}
}

func TestKeySelectDeselectInlinePR(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))

	runes := func(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

	fp.Update(m, runes('l'))
	if !fp.rightSelected {
		t.Fatalf("l should select the inline PR status")
	}
	fp.Update(m, runes('h'))
	if fp.rightSelected {
		t.Fatalf("h should deselect")
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	if !fp.rightSelected {
		t.Fatalf("right should select")
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyLeft})
	if fp.rightSelected {
		t.Fatalf("left should deselect")
	}
}

func TestKeySpaceOpensPRWhenSelected(t *testing.T) {
	// In the PR sub-mode, space/tab must open the PR like enter — not connect to
	// the session the cursor happens to sit on.
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(ref(5, "https://x/pull/5", "t")))
	fp.rightSelected = true
	cmd := fp.Update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if cmd == nil {
		t.Fatalf("space in the PR sub-mode should open the PR (non-nil cmd)")
	}
	if !strings.Contains(m.message, "5") {
		t.Errorf("space should open PR #5; message = %q", m.message)
	}
	if fp.mode != viewNormal {
		t.Errorf("single PR should not open the chooser, mode = %v", fp.mode)
	}
}

func TestKeySelectClearedByVerticalMove(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	fp.rightSelected = true
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}) // up to instance row
	if fp.rightSelected {
		t.Fatalf("vertical move should clear the selection")
	}
}

func TestKeyLWithoutInlinePRDoesNotSelect(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, nil) // no PR status anywhere
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.kind == rowNewSession {
			fp.cursor = i
		}
	}
	fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if fp.rightSelected {
		t.Fatalf("l on a row without an inline PR must not select")
	}
}

func TestBuildRowsClearsStaleSelection(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp, m := cursorOnInlinePR(t, inst, prStatusWithRefs(ref(1, "https://x/pull/1", "t")))
	fp.rightSelected = true
	// PR closes: clear the runtime PR status, rebuild — selection must drop.
	m.runtime[rtKey("alpha", "agent-1")] = &fleetgrpc.InstanceRuntime{Fleet: "alpha", Instance: "agent-1"}
	fp.buildRows(m)
	if fp.rightSelected {
		t.Fatalf("buildRows should clear selection once the inline PR is gone")
	}
}

func TestMouseClickOnInlinePROpensPR(t *testing.T) {
	// A left-click on the inline PR status column selects it AND opens the PR:
	// the mouse handler leaves synthesizedKey at its default (Enter), which the
	// tag-selected path turns into openSelectedPR.
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	mp := autoTagModel(fp, inst, true, prStatusWithRefs(ref(99, "https://x/pull/99", "fix")))
	m := *mp
	m.currentPage = fp
	m.width = 90
	fp.buildRows(&m)
	fp.viewFleetList(&m) // records listRowY

	rowIdx := -1
	for i, r := range fp.rows {
		if r.prStatusInline {
			rowIdx = i
		}
	}
	if rowIdx < 0 {
		t.Fatal("no inline-PR row built")
	}
	if fp.listRowY < 0 {
		t.Fatalf("listRowY not recorded")
	}

	click := tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      prStatusClickColumn,
		Y:      fp.listRowY + rowIdx,
	}
	next, _ := m.Update(click)
	nm := next.(model)
	if !nm.fleetPage.rightSelected {
		t.Errorf("click on the inline PR should select it")
	}
	if !strings.Contains(nm.message, "99") {
		t.Errorf("click should open PR #99 (message %q)", nm.message)
	}
}

func TestIsBrowsableURL(t *testing.T) {
	for _, u := range []string{"https://github.com/o/r/pull/1", "http://localhost:3000"} {
		if !isBrowsableURL(u) {
			t.Errorf("%q should be browsable", u)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://x", "", "not a url"} {
		if isBrowsableURL(u) {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestOpenExternalURLRefusesNonHTTP(t *testing.T) {
	// Must return an error WITHOUT launching anything for a non-http scheme.
	if err := openExternalURL("javascript:alert(1)"); err == nil {
		t.Errorf("expected refusal for a non-http(s) URL")
	}
}
