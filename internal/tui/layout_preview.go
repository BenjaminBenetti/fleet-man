package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// layout_preview.go renders a miniature, navigable picture of a saved tmux
// window layout for the layout-preset dialog (issue #150). The input is the
// same #{window_layout} string the group save/restore mechanism persists
// (savedGroup.Layout): leaf 0 is the pane select-layout assigns to the fleet
// TUI itself (pane index 0), and the remaining leaves — sorted by position,
// top then left, exactly like listPanesByPosition — are the session panes a
// preset assigns startup commands to.

// layoutRect is one leaf pane of a parsed tmux layout, in the cell coordinates
// of the window the layout was captured from.
type layoutRect struct {
	x, y, w, h int
}

// parseTmuxLayout parses a tmux #{window_layout} string into its leaf panes,
// in string order (which select-layout maps to pane-index order). The grammar:
//
//	layout := checksum ',' cell
//	cell   := WxH ',' X ',' Y ( ',' paneID | '{' cell (',' cell)* '}' | '[' cell (',' cell)* ']' )
//
// where '{}' is a left-right split and '[]' top-bottom. Container dims are
// discarded — only leaves are returned.
func parseTmuxLayout(layout string) ([]layoutRect, error) {
	s := strings.TrimSpace(layout)
	if s == "" {
		return nil, fmt.Errorf("empty layout")
	}
	// Strip the leading 4-hex-digit checksum ("b25d,...").
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, fmt.Errorf("layout %q: missing checksum separator", layout)
	}
	p := &layoutParser{s: s, pos: comma + 1}
	var leaves []layoutRect
	if err := p.parseCell(&leaves); err != nil {
		return nil, fmt.Errorf("layout %q: %w", layout, err)
	}
	if p.pos != len(p.s) {
		return nil, fmt.Errorf("layout %q: trailing garbage at %d", layout, p.pos)
	}
	if len(leaves) == 0 {
		return nil, fmt.Errorf("layout %q: no panes", layout)
	}
	return leaves, nil
}

type layoutParser struct {
	s   string
	pos int
}

func (p *layoutParser) parseCell(leaves *[]layoutRect) error {
	w, err := p.parseInt()
	if err != nil {
		return err
	}
	if err := p.expect('x'); err != nil {
		return err
	}
	h, err := p.parseInt()
	if err != nil {
		return err
	}
	if err := p.expect(','); err != nil {
		return err
	}
	x, err := p.parseInt()
	if err != nil {
		return err
	}
	if err := p.expect(','); err != nil {
		return err
	}
	y, err := p.parseInt()
	if err != nil {
		return err
	}

	switch p.peek() {
	case '{', '[':
		open := p.peek()
		close := byte('}')
		if open == '[' {
			close = ']'
		}
		p.pos++
		for {
			if err := p.parseCell(leaves); err != nil {
				return err
			}
			if p.peek() == ',' {
				p.pos++
				continue
			}
			break
		}
		if err := p.expect(close); err != nil {
			return err
		}
	case ',':
		// A leaf: ",paneID". The ID is the pane's %N number at capture time;
		// it is meaningless after a restore, so it is parsed and dropped.
		p.pos++
		if _, err := p.parseInt(); err != nil {
			return err
		}
		*leaves = append(*leaves, layoutRect{x: x, y: y, w: w, h: h})
	default:
		// A bare leaf with no pane ID (not produced by tmux, but harmless).
		*leaves = append(*leaves, layoutRect{x: x, y: y, w: w, h: h})
	}
	return nil
}

func (p *layoutParser) peek() byte {
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *layoutParser) expect(c byte) error {
	if p.peek() != c {
		return fmt.Errorf("expected %q at %d", string(c), p.pos)
	}
	p.pos++
	return nil
}

func (p *layoutParser) parseInt() (int, error) {
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == start {
		return 0, fmt.Errorf("expected number at %d", start)
	}
	return strconv.Atoi(p.s[start:p.pos])
}

// synthesizedStackRects builds the pane geometry a preset with no captured
// layout string produces when applied: restoreGroupCmd with an empty layout
// leaves the phase-1 placeholder splits in place — the TUI pane on the left
// (30%) and the session panes stacked vertically on the right. Leaf 0 is the
// TUI pane, matching parseTmuxLayout's convention.
func synthesizedStackRects(panes int) []layoutRect {
	if panes < 1 {
		panes = 1
	}
	const w, tuiW = 100, 30
	// Right column: panes cells of equal height with 1-cell separators.
	paneH := 8
	h := panes*paneH + (panes - 1)
	rects := []layoutRect{{x: 0, y: 0, w: tuiW, h: h}}
	for i := range panes {
		rects = append(rects, layoutRect{x: tuiW + 1, y: i * (paneH + 1), w: w - tuiW - 1, h: paneH})
	}
	return rects
}

// orderRectsByPosition returns the indices of the session panes (every leaf
// but leaf 0, the TUI pane) sorted by screen position, top then left — the
// same ordering listPanesByPosition gives the live panes, so slot i of the
// result is the pane that GroupLayout.Sessions[i] / LayoutPreset.PaneCommands[i]
// maps to at restore time.
func orderRectsByPosition(rects []layoutRect) []int {
	order := make([]int, 0, len(rects)-1)
	for i := 1; i < len(rects); i++ {
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := rects[order[a]], rects[order[b]]
		if ra.y != rb.y {
			return ra.y < rb.y
		}
		return ra.x < rb.x
	})
	return order
}

// Box-drawing assembly: each grid cell carries a bitmask of line directions
// and the mask resolves to the matching rune, so abutting pane borders fuse
// into ├ ┬ ┼ etc. instead of overdrawing each other.
const (
	lineUp = 1 << iota
	lineDown
	lineLeft
	lineRight
)

var lineRunes = map[int]rune{
	lineLeft | lineRight:                     '─',
	lineUp | lineDown:                        '│',
	lineDown | lineRight:                     '┌',
	lineDown | lineLeft:                      '┐',
	lineUp | lineRight:                       '└',
	lineUp | lineLeft:                        '┘',
	lineUp | lineDown | lineRight:            '├',
	lineUp | lineDown | lineLeft:             '┤',
	lineDown | lineLeft | lineRight:          '┬',
	lineUp | lineLeft | lineRight:            '┴',
	lineUp | lineDown | lineLeft | lineRight: '┼',
	lineUp:                                   '│',
	lineDown:                                 '│',
	lineLeft:                                 '─',
	lineRight:                                '─',
}

// previewBorders maps each pane to its scaled border-line coordinates on a
// charWidth x charHeight canvas. tmux pane cells abut across a 1-cell
// separator (a pane at x occupies [x, x+w) with the separator at x-1 / x+w),
// so the shared border line of two adjacent panes is the same window
// coordinate — left edge x-1 (or 0 at the window edge) and right edge x+w —
// which keeps neighbors' scaled borders coincident.
type previewBox struct {
	x0, y0, x1, y1 int // inclusive border-line coordinates on the canvas
}

func previewBorderBoxes(rects []layoutRect, charWidth, charHeight int) []previewBox {
	// Window extent in cells: the furthest right/bottom edge.
	winW, winH := 0, 0
	for _, r := range rects {
		winW = max(winW, r.x+r.w)
		winH = max(winH, r.y+r.h)
	}
	scaleX := func(b int) int { return b * (charWidth - 1) / max(winW, 1) }
	scaleY := func(b int) int { return b * (charHeight - 1) / max(winH, 1) }

	boxes := make([]previewBox, 0, len(rects))
	for _, r := range rects {
		left, top := 0, 0
		if r.x > 0 {
			left = r.x - 1
		}
		if r.y > 0 {
			top = r.y - 1
		}
		boxes = append(boxes, previewBox{
			x0: scaleX(left),
			y0: scaleY(top),
			x1: scaleX(r.x + r.w),
			y1: scaleY(r.y + r.h),
		})
	}
	return boxes
}

// renderLayoutPreview draws the pane layout as a charWidth x charHeight
// box-drawing diagram. Leaf 0 is labeled as the fleet TUI pane; session panes
// are labeled by their 1-based slot number (position order, see
// orderRectsByPosition) with a ✓ once their command slot has been assigned.
// selectedSlot (an index into the slot order, -1 for none) highlights that
// pane's interior.
func renderLayoutPreview(rects []layoutRect, order []int, selectedSlot int, assigned []bool, charWidth, charHeight int) string {
	if len(rects) == 0 || charWidth < 8 || charHeight < 3 {
		return ""
	}
	boxes := previewBorderBoxes(rects, charWidth, charHeight)

	masks := make([][]int, charHeight)
	for i := range masks {
		masks[i] = make([]int, charWidth)
	}
	for _, b := range boxes {
		for x := b.x0; x < b.x1; x++ {
			masks[b.y0][x] |= lineRight
			masks[b.y1][x] |= lineRight
			masks[b.y0][x+1] |= lineLeft
			masks[b.y1][x+1] |= lineLeft
		}
		for y := b.y0; y < b.y1; y++ {
			masks[y][b.x0] |= lineDown
			masks[y][b.x1] |= lineDown
			masks[y+1][b.x0] |= lineUp
			masks[y+1][b.x1] |= lineUp
		}
	}

	grid := make([][]rune, charHeight)
	for y := range grid {
		grid[y] = make([]rune, charWidth)
		for x := range grid[y] {
			if r, ok := lineRunes[masks[y][x]]; ok {
				grid[y][x] = r
			} else {
				grid[y][x] = ' '
			}
		}
	}

	// Slot lookup: rect index -> slot number (0-based), -1 for the TUI pane.
	slotOf := make(map[int]int, len(order))
	for slot, rectIdx := range order {
		slotOf[rectIdx] = slot
	}

	// Labels, centered in each pane's interior.
	for i, b := range boxes {
		label := "fleet"
		if i > 0 {
			slot := slotOf[i]
			label = strconv.Itoa(slot + 1)
			if slot < len(assigned) && assigned[slot] {
				label += " ✓"
			}
		}
		placeLabel(grid, b, label)
	}

	// Assemble lines, highlighting the selected pane's interior (borders
	// included so the selection reads as a frame around the pane).
	var selBox *previewBox
	if selectedSlot >= 0 && selectedSlot < len(order) {
		selBox = &boxes[order[selectedSlot]]
	}
	var lines []string
	for y := range charHeight {
		if selBox == nil || y < selBox.y0 || y > selBox.y1 {
			lines = append(lines, string(grid[y]))
			continue
		}
		row := grid[y]
		var sb strings.Builder
		sb.WriteString(string(row[:selBox.x0]))
		sb.WriteString(selectedStyle.Render(string(row[selBox.x0 : selBox.x1+1])))
		sb.WriteString(string(row[selBox.x1+1:]))
		lines = append(lines, sb.String())
	}
	return strings.Join(lines, "\n")
}

// placeLabel writes label centered inside box b's interior, truncating to fit;
// a pane too small for even one character keeps its blank interior.
func placeLabel(grid [][]rune, b previewBox, label string) {
	innerW := b.x1 - b.x0 - 1
	innerH := b.y1 - b.y0 - 1
	if innerW < 1 || innerH < 1 {
		return
	}
	runes := []rune(label)
	if len(runes) > innerW {
		runes = runes[:innerW]
	}
	y := b.y0 + 1 + (innerH-1)/2
	x := b.x0 + 1 + (innerW-len(runes))/2
	copy(grid[y][x:], runes)
}

// previewDims picks the preview canvas size for a pane set: wide enough for
// the 44-column dialog interior, and tall enough that each stacked pane keeps
// a usable interior (3 rows per vertical level, bounded so a deep stack still
// fits on screen).
func previewDims(rects []layoutRect) (int, int) {
	levels := map[int]bool{}
	for _, r := range rects {
		levels[r.y] = true
	}
	height := min(max(9, 3*len(levels)+1), 19)
	return 42, height
}
