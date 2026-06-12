package tui

import (
	"strings"
	"testing"
)

// Captured from a real tmux server (208x58 window): TUI pane on the left,
// right column split into two stacked panes.
const sampleLayout = "a0c2,208x58,0,0{104x58,0,0,0,103x58,105,0[103x29,105,0,1,103x28,105,30,2]}"

func TestParseTmuxLayoutNested(t *testing.T) {
	rects, err := parseTmuxLayout(sampleLayout)
	if err != nil {
		t.Fatalf("parseTmuxLayout: %v", err)
	}
	want := []layoutRect{
		{x: 0, y: 0, w: 104, h: 58},
		{x: 105, y: 0, w: 103, h: 29},
		{x: 105, y: 30, w: 103, h: 28},
	}
	if len(rects) != len(want) {
		t.Fatalf("got %d leaves, want %d: %v", len(rects), len(want), rects)
	}
	for i, w := range want {
		if rects[i] != w {
			t.Fatalf("leaf %d = %+v, want %+v", i, rects[i], w)
		}
	}
}

func TestParseTmuxLayoutSinglePane(t *testing.T) {
	rects, err := parseTmuxLayout("c3d4,80x24,0,0,5")
	if err != nil {
		t.Fatalf("parseTmuxLayout: %v", err)
	}
	if len(rects) != 1 || rects[0] != (layoutRect{x: 0, y: 0, w: 80, h: 24}) {
		t.Fatalf("unexpected leaves: %v", rects)
	}
}

func TestParseTmuxLayoutRejectsGarbage(t *testing.T) {
	for _, layout := range []string{
		"",
		"nochecksum",
		"abcd,80x24,0,0,5trailing",
		"abcd,80x24,0,0{40x24,0,0,1,39x24,41,0,2", // unclosed brace
		"abcd,80x,0,0,5",
	} {
		if _, err := parseTmuxLayout(layout); err == nil {
			t.Fatalf("parseTmuxLayout(%q): want error", layout)
		}
	}
}

func TestOrderRectsByPositionTopThenLeft(t *testing.T) {
	// Leaves in capture order: TUI, bottom-right, top-right — position order
	// must come back top-right (y=0) before bottom-right (y=30).
	rects := []layoutRect{
		{x: 0, y: 0, w: 104, h: 58},    // TUI (excluded)
		{x: 105, y: 30, w: 103, h: 28}, // bottom
		{x: 105, y: 0, w: 103, h: 29},  // top
	}
	order := orderRectsByPosition(rects)
	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("order = %v, want [2 1]", order)
	}
}

func TestSynthesizedStackRects(t *testing.T) {
	rects := synthesizedStackRects(3)
	if len(rects) != 4 {
		t.Fatalf("got %d rects, want 4 (TUI + 3 panes)", len(rects))
	}
	order := orderRectsByPosition(rects)
	// Stacked panes must come back in top-to-bottom order.
	for i := 1; i < len(order); i++ {
		if rects[order[i]].y <= rects[order[i-1]].y {
			t.Fatalf("stack not top-to-bottom: %v", rects)
		}
	}
}

func TestRenderLayoutPreviewLabelsAndChecks(t *testing.T) {
	rects, err := parseTmuxLayout(sampleLayout)
	if err != nil {
		t.Fatalf("parseTmuxLayout: %v", err)
	}
	order := orderRectsByPosition(rects)
	out := renderLayoutPreview(rects, order, -1, []bool{true, false}, 42, 12)

	if !strings.Contains(out, "fleet") {
		t.Fatalf("preview missing TUI pane label:\n%s", out)
	}
	if !strings.Contains(out, "1 ✓") {
		t.Fatalf("preview missing assigned slot-1 label:\n%s", out)
	}
	if !strings.Contains(out, "2") {
		t.Fatalf("preview missing slot-2 label:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 12 {
		t.Fatalf("preview height = %d lines, want 12", len(lines))
	}
}

func TestRenderLayoutPreviewSharedBordersFuse(t *testing.T) {
	rects, err := parseTmuxLayout(sampleLayout)
	if err != nil {
		t.Fatalf("parseTmuxLayout: %v", err)
	}
	out := renderLayoutPreview(rects, orderRectsByPosition(rects), -1, nil, 42, 12)
	// The right column's two panes share a horizontal border that meets the
	// TUI/right vertical border in a ├ junction; the outer corners must be
	// rounded off as plain corners.
	for _, r := range []string{"┌", "┐", "└", "┘", "├"} {
		if !strings.Contains(out, r) {
			t.Fatalf("preview missing %q:\n%s", r, out)
		}
	}
}
