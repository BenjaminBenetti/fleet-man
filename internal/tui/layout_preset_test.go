package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestBuildPresetSessionScript(t *testing.T) {
	script := buildPresetSessionScript(
		[]string{"inst~dev", "inst~dev~a1"},
		[]string{"npm run dev", ""},
	)
	if !strings.Contains(script, `tmux new-session -d -s 'inst~dev' 2>/dev/null`) {
		t.Fatalf("missing root new-session: %s", script)
	}
	if !strings.Contains(script, `tmux new-session -d -s 'inst~dev~a1' 2>/dev/null`) {
		t.Fatalf("missing pane new-session: %s", script)
	}
	// The first pane's command is typed literally (-l) after a "--" flag
	// terminator (so a leading-dash command isn't parsed as a flag) then
	// submitted; the exact (=, colon-terminated) session target prevents tmux
	// prefix-matching from hitting inst~dev~a1.
	if !strings.Contains(script, `tmux send-keys -t '=inst~dev:' -l -- 'npm run dev'`) {
		t.Fatalf("missing literal send-keys: %s", script)
	}
	if !strings.Contains(script, `tmux send-keys -t '=inst~dev:' Enter`) {
		t.Fatalf("missing Enter send-keys: %s", script)
	}
	// The empty second command must not produce a send-keys for that pane.
	if strings.Contains(script, `'=inst~dev~a1:'`) {
		t.Fatalf("plain-shell pane got a send-keys: %s", script)
	}
	// Steps are &&-chained so a name collision aborts instead of minting a
	// partial group.
	if !strings.Contains(script, "&&") {
		t.Fatalf("steps not chained: %s", script)
	}
}

func TestCyclePresetTemplateWrapsThroughNone(t *testing.T) {
	page := newFleetPage()
	page.dialogPresets = []fleet.LayoutPreset{
		{Name: "a", PaneCommands: []string{""}},
		{Name: "b", PaneCommands: []string{""}},
	}
	page.dialogPresetIdx = -1

	want := []int{0, 1, -1, 0}
	for i, w := range want {
		page.cyclePresetTemplate(1)
		if page.dialogPresetIdx != w {
			t.Fatalf("step %d: idx = %d, want %d", i, page.dialogPresetIdx, w)
		}
	}
	// And backwards from 0 → -1 → 1.
	page.dialogPresetIdx = 0
	page.cyclePresetTemplate(-1)
	if page.dialogPresetIdx != -1 {
		t.Fatalf("backward from 0: idx = %d, want -1", page.dialogPresetIdx)
	}
	page.cyclePresetTemplate(-1)
	if page.dialogPresetIdx != 1 {
		t.Fatalf("backward from -1: idx = %d, want 1", page.dialogPresetIdx)
	}
}

func TestCyclePresetTemplateNoPresetsIsNoop(t *testing.T) {
	page := newFleetPage()
	page.dialogPresetIdx = -1
	page.cyclePresetTemplate(1)
	if page.dialogPresetIdx != -1 {
		t.Fatalf("idx = %d, want -1", page.dialogPresetIdx)
	}
}

func TestUniquePresetName(t *testing.T) {
	presets := []fleet.LayoutPreset{
		{Name: "dev"},
		{Name: "dev-2"},
	}
	if got := uniquePresetName("dev", presets, -1); got != "dev-3" {
		t.Fatalf("got %q, want dev-3", got)
	}
	if got := uniquePresetName("fresh", presets, -1); got != "fresh" {
		t.Fatalf("got %q, want fresh", got)
	}
	// Editing preset 0 may keep its own name.
	if got := uniquePresetName("dev", presets, 0); got != "dev" {
		t.Fatalf("got %q, want dev (own name)", got)
	}
	if got := uniquePresetName("", nil, -1); got != "layout" {
		t.Fatalf("got %q, want layout fallback", got)
	}
}

func TestInitEditStageFallsBackOnPaneCountMismatch(t *testing.T) {
	lp := &layoutPresetFlow{editIdx: -1}
	// sampleLayout has 2 session panes; claiming 3 must drop the geometry and
	// synthesize a 3-pane stack so the preview matches what apply would do.
	lp.initEditStage(sampleLayout, 3, "dev", nil)
	if lp.layout != "" {
		t.Fatalf("mismatched layout kept: %q", lp.layout)
	}
	if lp.paneCount() != 3 {
		t.Fatalf("paneCount = %d, want 3", lp.paneCount())
	}
}

func TestInitEditStageParsesMatchingLayout(t *testing.T) {
	lp := &layoutPresetFlow{editIdx: -1}
	lp.initEditStage(sampleLayout, 2, "dev", nil)
	if lp.layout != sampleLayout {
		t.Fatalf("layout dropped: %q", lp.layout)
	}
	if lp.paneCount() != 2 {
		t.Fatalf("paneCount = %d, want 2", lp.paneCount())
	}
	if len(lp.rects) != 3 {
		t.Fatalf("rects = %d, want 3 (TUI + 2)", len(lp.rects))
	}
	if lp.focus != 1 {
		t.Fatalf("focus = %d, want first pane", lp.focus)
	}
}

func TestInitEditStageCarriesOverExistingCommands(t *testing.T) {
	lp := &layoutPresetFlow{editIdx: 0}
	lp.initEditStage(sampleLayout, 2, "dev", []string{"htop", ""})
	if lp.commands[0] != "htop" || lp.commands[1] != "" {
		t.Fatalf("commands not carried over: %v", lp.commands)
	}
	// The ✓ marks panes carrying a command; the empty pane is a plain shell.
	flags := lp.paneCommandFlags()
	if !flags[0] || flags[1] {
		t.Fatalf("paneCommandFlags = %v, want [true false]", flags)
	}
}

func TestLayoutPresetFocusCycleIncludesSaveUngated(t *testing.T) {
	lp := &layoutPresetFlow{editIdx: -1}
	lp.initEditStage("", 2, "dev", nil)

	// Cycle name(0) → pane1(1) → pane2(2) → cancel(3) → save(4) → name(0).
	// The save stop is always present — no gating on assigning commands.
	lp.focus = lpFocusName
	seen := []int{}
	for range 5 {
		lp.moveFocus(1)
		seen = append(seen, lp.focus)
	}
	want := []int{1, 2, 3, 4, 0}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("focus cycle = %v, want %v", seen, want)
		}
	}
	if lp.focusConfirm() != 4 {
		t.Fatalf("focusConfirm = %d, want 4", lp.focusConfirm())
	}
}

func TestAdvanceAfterCommand(t *testing.T) {
	lp := &layoutPresetFlow{editIdx: -1}
	lp.initEditStage("", 3, "dev", nil)
	// From pane 1 (focus=1) → pane 2 (focus=2).
	lp.focus = 1
	lp.advanceAfterCommand()
	if lp.focusedSlot() != 1 {
		t.Fatalf("slot = %d, want 1", lp.focusedSlot())
	}
	// From the last pane → save.
	lp.focus = lp.paneCount() // focus on last pane (slot paneCount-1)
	lp.advanceAfterCommand()
	if lp.focus != lp.focusConfirm() {
		t.Fatalf("focus = %d, want confirm", lp.focus)
	}
}
