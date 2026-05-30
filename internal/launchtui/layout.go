package launchtui

import "github.com/charmbracelet/lipgloss"

// ===========================================
// Pure layout geometry
// ===========================================
//
// The grid is laid out by pure functions so the exact same geometry drives both
// rendering (View) and mouse hit-testing (Update). If View and the mouse handler
// computed positions independently they would inevitably drift and a click would
// land on the wrong pill; sharing one gridLayout makes that impossible.
//
// Each item renders as a compact single-line "pill": its title text plus a
// little horizontal padding, on a coloured background — no border, no subtitle.
// Pills are sized to their content (not a fixed grid cell), so a row packs as
// many as fit across the available width and then wraps. Coordinates are
// character cells, origin (0,0) at the top-left of the rendered view; the title
// and section headers occupy whole rows above the pills and their height is
// baked into every pill's Y so the mouse handler can map a raw (X,Y) straight to
// an item.

// section identifies which of the two labelled groups an item belongs to.
type section int

const (
	// sectionLink is the "Links" group (configured fleetLaunch.sites).
	sectionLink section = iota
	// sectionApp is the "Apps" group (configured fleetLaunch.apps).
	sectionApp
)

// Layout sizing constants.
const (
	// pillPadding is the horizontal padding inside a pill, on each side of the
	// title. A pill's rendered width is the title's display width plus twice
	// this.
	pillPadding = 1
	// pillGap is the blank space between two adjacent pills on a row.
	pillGap = 1
	// headerRows is the height a section consumes above its pills: the bold
	// label line plus the horizontal divider line beneath it.
	headerRows = 2
	// horizontalMargin is the blank space kept at the left so pills don't butt
	// against the edge; the column-fit budget reserves it on the right too.
	horizontalMargin = 2
)

// rowStride is the vertical distance between the tops of two consecutive pill
// rows. Pills are a single line and are stacked with no gap between rows, so
// successive rows are adjacent — keeping the grid as vertically compact as
// possible.
const rowStride = 1

// rect is an axis-aligned rectangle in character cells: its top-left corner
// (X,Y) and its size (W,H). Contains reports whether a point falls inside.
type rect struct {
	X, Y, W, H int
}

// contains reports whether the cell at (px,py) lies within r (inclusive of the
// top/left edge, exclusive of the bottom/right edge — standard half-open box).
func (r rect) contains(px, py int) bool {
	return px >= r.X && px < r.X+r.W && py >= r.Y && py < r.Y+r.H
}

// itemPlacement is the resolved position of one selectable item: which section
// it is in, its row/column within that section's wrapped grid, the label its
// pill shows (already truncated to fit), and the screen-space rectangle of the
// pill. Row/col are section-relative; the rectangle is absolute (it already
// accounts for the title and section headers stacked above it).
type itemPlacement struct {
	section section
	row     int
	col     int
	label   string
	rect    rect
}

// gridLayout is the fully resolved geometry of one render: the placement of
// every selectable item, indexed by the item's position in the flattened
// links-then-apps order. View walks placements to draw pills; the mouse handler
// scans them to map a click to an item index. Both read the same slice, so they
// can never disagree.
type gridLayout struct {
	// placements holds one entry per item, in flattened order (all links, then
	// all apps). placements[i] is the geometry of item i.
	placements []itemPlacement
}

// pillLabel returns the text shown in an item's pill, truncated so the rendered
// pill (label + padding) never exceeds maxWidth columns. maxWidth is the
// available content width for a whole row, so an absurdly long title degrades to
// a truncated single pill rather than overflowing the line.
func pillLabel(it item, maxWidth int) string {
	capacity := maxWidth - 2*pillPadding
	if capacity < 1 {
		capacity = 1
	}
	return truncate(itemTitle(it), capacity)
}

// pillWidth is the rendered width of a pill showing label: its display width
// plus the padding on both sides.
func pillWidth(label string) int {
	return lipgloss.Width(label) + 2*pillPadding
}

// layout resolves the geometry for the given items rendered at the given
// terminal width, where the first `links` items are the Links section and the
// rest are Apps. Pills flow left to right and wrap when the next one wouldn't
// fit; the Apps section is stacked below all Link rows plus both headers.
//
// Vertical stacking, top to bottom:
//
//	"Links" label + divider  (headerRows)
//	link pills               (linkRows rows, rowStride each)
//	"Apps" label + divider   (headerRows)
//	app pills                (appRows rows, rowStride each)
//
// The Apps section always sits below the Links section even when one side is
// empty (an empty section contributes zero pill rows), matching the
// two-headers-always layout in View.
func layout(width int, items []item, links int) gridLayout {
	if links < 0 {
		links = 0
	}
	if links > len(items) {
		links = len(items)
	}

	avail := width - 2*horizontalMargin
	if avail < 1 {
		avail = 1
	}

	gl := gridLayout{placements: make([]itemPlacement, 0, len(items))}

	linkTop := headerRows
	linkPlacements, linkRows := layoutSection(items[:links], sectionLink, avail, linkTop)

	appsTop := linkTop + linkRows*rowStride + headerRows
	appPlacements, _ := layoutSection(items[links:], sectionApp, avail, appsTop)

	gl.placements = append(gl.placements, linkPlacements...)
	gl.placements = append(gl.placements, appPlacements...)
	return gl
}

// layoutSection lays one section's items into wrapping rows whose first row
// begins at screen row sectionTop. avail is the usable content width (terminal
// width minus margins). It returns the placements (in item order) and the number
// of pill rows the section occupies (zero when it has no items).
func layoutSection(items []item, sec section, avail, sectionTop int) ([]itemPlacement, int) {
	placements := make([]itemPlacement, 0, len(items))
	x, row, col := 0, 0, 0
	for _, it := range items {
		label := pillLabel(it, avail)
		pillW := pillWidth(label)
		// Wrap when this pill wouldn't fit on the current row (but never wrap a
		// row that has no pills yet — a single over-wide pill just gets its own
		// row, clamped by pillLabel).
		if col > 0 && x+pillW > avail {
			row++
			col = 0
			x = 0
		}
		placements = append(placements, itemPlacement{
			section: sec,
			row:     row,
			col:     col,
			label:   label,
			rect:    rect{X: horizontalMargin + x, Y: sectionTop + row*rowStride, W: pillW, H: 1},
		})
		x += pillW + pillGap
		col++
	}
	rows := 0
	if len(placements) > 0 {
		rows = row + 1
	}
	return placements, rows
}

// hitTest maps a point to the index of the item whose pill contains it, or -1
// when the point falls on a gap, header, or empty area. The flattened index it
// returns is directly assignable to the model's cursor.
func (gl gridLayout) hitTest(px, py int) int {
	for i, p := range gl.placements {
		if p.rect.contains(px, py) {
			return i
		}
	}
	return -1
}
