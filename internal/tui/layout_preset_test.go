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
	// 2>&1 (not 2>/dev/null): a "duplicate session" reason must survive so the
	// TUI can show it instead of a bare "exit status 1".
	if !strings.Contains(script, `tmux new-session -d -s 'inst~dev' 2>&1`) {
		t.Fatalf("missing root new-session: %s", script)
	}
	if !strings.Contains(script, `tmux new-session -d -s 'inst~dev~a1' 2>&1`) {
		t.Fatalf("missing pane new-session: %s", script)
	}
	if strings.Contains(script, "2>/dev/null tmux new-session") || strings.Contains(script, `new-session -d -s 'inst~dev' 2>/dev/null`) {
		t.Fatalf("new-session still swallows stderr: %s", script)
	}
	// The first pane's command is typed literally (-l) after a "--" flag
	// terminator (so a leading-dash command isn't parsed as a flag) then
	// submitted; the exact (=, colon-terminated) session target prevents tmux
	// prefix-matching from hitting inst~dev~a1. send-keys also merges stderr so a
	// failure HERE (not just at new-session) surfaces its reason, not "exit 1".
	if !strings.Contains(script, `tmux send-keys -t '=inst~dev:' -l -- 'npm run dev' 2>&1`) {
		t.Fatalf("missing literal send-keys: %s", script)
	}
	if !strings.Contains(script, `tmux send-keys -t '=inst~dev:' Enter 2>&1`) {
		t.Fatalf("missing Enter send-keys: %s", script)
	}
	// The empty second command must not produce a send-keys for that pane.
	if strings.Contains(script, `send-keys -t '=inst~dev~a1:'`) {
		t.Fatalf("plain-shell pane got a send-keys: %s", script)
	}
	// Steps are &&-chained so a later failure aborts instead of minting a
	// partial group.
	if !strings.Contains(script, "&&") {
		t.Fatalf("steps not chained: %s", script)
	}
	// The root is created on its own and gated with `|| exit 1`, so a collision
	// on the EXISTING root exits before the cleanup — a pre-existing session is
	// never killed.
	if !strings.Contains(script, `tmux new-session -d -s 'inst~dev' 2>&1 || exit 1`) {
		t.Fatalf("root not gated with || exit 1: %s", script)
	}
	// A failure after the root exists tears down every session this run made,
	// so a partial chain can't strand a root that collides forever on retry.
	if !strings.Contains(script, `tmux kill-session -t '=inst~dev:' 2>/dev/null`) ||
		!strings.Contains(script, `tmux kill-session -t '=inst~dev~a1:' 2>/dev/null`) {
		t.Fatalf("missing partial-failure cleanup: %s", script)
	}
}

// A single-pane plain-shell preset has nothing to run after the root, so it is
// just the bare guarded new-session — no cleanup block (there is nothing to tear
// down) and no stray `|| exit 1`/kill scaffolding.
func TestBuildPresetSessionScriptSinglePaneNoCleanup(t *testing.T) {
	script := buildPresetSessionScript([]string{"inst~dev"}, []string{""})
	if !strings.Contains(script, `tmux new-session -d -s 'inst~dev' 2>&1`) {
		t.Fatalf("missing root new-session: %s", script)
	}
	if strings.Contains(script, "kill-session") || strings.Contains(script, "exit 1") {
		t.Fatalf("single bare session should have no cleanup scaffolding: %s", script)
	}
}

func TestGroupIDFor(t *testing.T) {
	existing := []tmuxSession{
		{Name: "inst~dev"},      // root of a live group "dev"
		{Name: "inst~dev~a1b2"}, // a pane of the same group
		{Name: "other"},         // ungrouped, ignored
	}
	// A free name is honored verbatim so the group reads as the preset/typed name.
	if got := groupIDFor("inst", "fromtpl", existing); got != "fromtpl" {
		t.Fatalf("got %q, want fromtpl (free name honored)", got)
	}
	// A taken name falls back to a random 6-char hex id — NOT a "dev-2" suffix
	// (which would prefix-match the live "dev" group in prefix-based group ops).
	got := groupIDFor("inst", "dev", existing)
	if got == "dev" || got == "dev-2" {
		t.Fatalf("got %q, want a random fallback id", got)
	}
	if len(got) != 6 {
		t.Fatalf("fallback %q is not a 3-byte hex id", got)
	}
	// A different instance's sessions never count against this instance.
	if got := groupIDFor("inst", "dev", []tmuxSession{{Name: "elsewhere~dev"}}); got != "dev" {
		t.Fatalf("got %q, want dev (other instance ignored)", got)
	}
}

func TestCyclePresetTemplateWrapsThroughNone(t *testing.T) {
	page := newFleetPage()
	page.newSession.presets = []fleet.LayoutPreset{
		{Name: "a", PaneCommands: []string{""}},
		{Name: "b", PaneCommands: []string{""}},
	}
	page.newSession.presetIdx = -1

	want := []int{0, 1, -1, 0}
	for i, w := range want {
		page.cyclePresetTemplate(1)
		if page.newSession.presetIdx != w {
			t.Fatalf("step %d: idx = %d, want %d", i, page.newSession.presetIdx, w)
		}
	}
	// And backwards from 0 → -1 → 1.
	page.newSession.presetIdx = 0
	page.cyclePresetTemplate(-1)
	if page.newSession.presetIdx != -1 {
		t.Fatalf("backward from 0: idx = %d, want -1", page.newSession.presetIdx)
	}
	page.cyclePresetTemplate(-1)
	if page.newSession.presetIdx != 1 {
		t.Fatalf("backward from -1: idx = %d, want 1", page.newSession.presetIdx)
	}
}

func TestCyclePresetTemplateNoPresetsIsNoop(t *testing.T) {
	page := newFleetPage()
	page.newSession.presetIdx = -1
	page.cyclePresetTemplate(1)
	if page.newSession.presetIdx != -1 {
		t.Fatalf("idx = %d, want -1", page.newSession.presetIdx)
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

// twoByTwoFlow builds a flow whose session panes form a 2x2 grid (plus the TUI
// pane on the left), so spatial navigation has both rows and columns to move
// between. Slots (position order, top-then-left): 0=TL, 1=TR, 2=BL, 3=BR.
func twoByTwoFlow() *layoutPresetFlow {
	lp := &layoutPresetFlow{editIdx: -1}
	lp.rects = []layoutRect{
		{x: 0, y: 0, w: 30, h: 20},  // TUI pane (leaf 0)
		{x: 31, y: 0, w: 34, h: 10}, // top-left
		{x: 66, y: 0, w: 34, h: 10}, // top-right
		{x: 31, y: 11, w: 34, h: 9}, // bottom-left
		{x: 66, y: 11, w: 34, h: 9}, // bottom-right
	}
	lp.order = orderRectsByPosition(lp.rects)
	lp.commands = make([]string, 4)
	return lp
}

func TestSpatialNavStaysInColumnAndRow(t *testing.T) {
	lp := twoByTwoFlow()

	// Down from top-left must reach bottom-left (same column), not top-right.
	lp.focus = 1 // slot 0 (TL)
	lp.navVertical(1)
	if lp.focusedSlot() != 2 {
		t.Fatalf("down from TL → slot %d, want 2 (BL)", lp.focusedSlot())
	}

	// Right from top-left must reach top-right (same row).
	lp.focus = 1 // slot 0 (TL)
	lp.navHorizontal(1)
	if lp.focusedSlot() != 1 {
		t.Fatalf("right from TL → slot %d, want 1 (TR)", lp.focusedSlot())
	}

	// Down from top-right → bottom-right.
	lp.focus = 2 // slot 1 (TR)
	lp.navVertical(1)
	if lp.focusedSlot() != 3 {
		t.Fatalf("down from TR → slot %d, want 3 (BR)", lp.focusedSlot())
	}

	// Up from bottom-left → top-left.
	lp.focus = 3 // slot 2 (BL)
	lp.navVertical(-1)
	if lp.focusedSlot() != 0 {
		t.Fatalf("up from BL → slot %d, want 0 (TL)", lp.focusedSlot())
	}

	// Left from bottom-right → bottom-left.
	lp.focus = 4 // slot 3 (BR)
	lp.navHorizontal(-1)
	if lp.focusedSlot() != 2 {
		t.Fatalf("left from BR → slot %d, want 2 (BL)", lp.focusedSlot())
	}
}

func TestSpatialNavRegionTransitions(t *testing.T) {
	lp := twoByTwoFlow()

	// Name → down → top-left pane.
	lp.focus = lpFocusName
	lp.navVertical(1)
	if lp.focusedSlot() != 0 {
		t.Fatalf("name down → slot %d, want 0 (TL)", lp.focusedSlot())
	}

	// Up from a top-row pane → name.
	lp.focus = 1 // TL
	lp.navVertical(-1)
	if lp.focus != lpFocusName {
		t.Fatalf("up from TL → focus %d, want name", lp.focus)
	}

	// Down from bottom-right (no pane below) → save (right-side button).
	lp.focus = 4 // BR
	lp.navVertical(1)
	if lp.focus != lp.focusConfirm() {
		t.Fatalf("down from BR → focus %d, want save", lp.focus)
	}

	// Down from bottom-left → cancel (left-side button).
	lp.focus = 3 // BL
	lp.navVertical(1)
	if lp.focus != lp.focusCancel() {
		t.Fatalf("down from BL → focus %d, want cancel", lp.focus)
	}

	// Cancel ↔ save via horizontal.
	lp.focus = lp.focusCancel()
	lp.navHorizontal(1)
	if lp.focus != lp.focusConfirm() {
		t.Fatalf("right from cancel → focus %d, want save", lp.focus)
	}
	lp.navHorizontal(-1)
	if lp.focus != lp.focusCancel() {
		t.Fatalf("left from save → focus %d, want cancel", lp.focus)
	}

	// Up from save → bottom-right pane (nearest its side).
	lp.focus = lp.focusConfirm()
	lp.navVertical(-1)
	if lp.focusedSlot() != 3 {
		t.Fatalf("up from save → slot %d, want 3 (BR)", lp.focusedSlot())
	}
	// Up from cancel → bottom-left pane.
	lp.focus = lp.focusCancel()
	lp.navVertical(-1)
	if lp.focusedSlot() != 2 {
		t.Fatalf("up from cancel → slot %d, want 2 (BL)", lp.focusedSlot())
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
