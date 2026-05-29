package launchtui

import "testing"

// mkItems builds a flattened item slice from link titles followed by app
// titles, returning it together with the link count the layout/model need.
func mkItems(linkTitles, appTitles []string) ([]item, int) {
	var items []item
	for _, t := range linkTitles {
		items = append(items, item{kind: kindLink, title: t})
	}
	for _, t := range appTitles {
		items = append(items, item{kind: kindApp, title: t})
	}
	return items, len(linkTitles)
}

// twoColTestWidth is a terminal width chosen so exactly two width-2 titles fit
// per row: each pill is 4 wide (title 2 + pillPadding on each side), so with
// avail = width - 2*horizontalMargin = 10, pills 0 and 1 fit (x+w = 9 ≤ 10) and
// a third would overflow (14 > 10) and wrap.
const twoColTestWidth = 2*horizontalMargin + 10

// TestLayoutWrapAndSections checks each item's section/row/col assignment for a
// grid that wraps, the Links-start offset, and the Apps section being stacked
// below the Links rows plus both headers.
func TestLayoutWrapAndSections(t *testing.T) {
	items, links := mkItems([]string{"AA", "BB", "CC"}, []string{"DD", "EE"})
	gl := layout(twoColTestWidth, items, links)

	if len(gl.placements) != 5 {
		t.Fatalf("placements = %d, want 5", len(gl.placements))
	}

	type rc struct {
		sec      section
		row, col int
	}
	want := []rc{
		{sectionLink, 0, 0},
		{sectionLink, 0, 1},
		{sectionLink, 1, 0}, // wrapped
		{sectionApp, 0, 0},
		{sectionApp, 0, 1},
	}
	for i, w := range want {
		p := gl.placements[i]
		if p.section != w.sec || p.row != w.row || p.col != w.col {
			t.Errorf("item %d = {sec:%d row:%d col:%d}, want {sec:%d row:%d col:%d}",
				i, p.section, p.row, p.col, w.sec, w.row, w.col)
		}
	}

	// Links start just below the "Links" label + divider (headerRows).
	if got, want := gl.placements[0].rect.Y, headerRows; got != want {
		t.Errorf("first link Y = %d, want %d", got, want)
	}

	// Apps begin below the two Links rows plus the "Apps" label + divider.
	linkRows := 2
	wantAppsY := headerRows + linkRows*rowStride + headerRows
	if got := gl.placements[3].rect.Y; got != wantAppsY {
		t.Errorf("first app Y = %d, want %d", got, wantAppsY)
	}
}

// TestLayoutEmptyLinks verifies the Apps offset is stable when there are no
// links: the empty Links section contributes zero pill rows, so Apps sit
// directly below both headers.
func TestLayoutEmptyLinks(t *testing.T) {
	items, links := mkItems(nil, []string{"DD", "EE"})
	gl := layout(twoColTestWidth, items, links)

	if len(gl.placements) != 2 {
		t.Fatalf("placements = %d, want 2", len(gl.placements))
	}
	wantAppsY := headerRows + 0*rowStride + headerRows
	if got := gl.placements[0].rect.Y; got != wantAppsY {
		t.Errorf("first app Y with no links = %d, want %d", got, wantAppsY)
	}
}

// TestPillWidthFromLabel verifies a pill's recorded width is the label's display
// width plus padding on both sides, and that an over-wide title is truncated to
// fit the available width rather than overflowing.
func TestPillWidthFromLabel(t *testing.T) {
	items, links := mkItems([]string{"Grafana"}, nil)
	gl := layout(80, items, links)
	if got, want := gl.placements[0].rect.W, len("Grafana")+2*pillPadding; got != want {
		t.Errorf("pill width = %d, want %d", got, want)
	}
	if got := gl.placements[0].label; got != "Grafana" {
		t.Errorf("label = %q, want %q", got, "Grafana")
	}

	// A title far wider than the terminal is truncated so the pill fits avail.
	narrow := 2*horizontalMargin + 8 // avail = 8
	items2, links2 := mkItems([]string{"ThisIsAVeryLongTitle"}, nil)
	gl2 := layout(narrow, items2, links2)
	if got := gl2.placements[0].rect.W; got > 8 {
		t.Errorf("over-wide pill width = %d, want ≤ avail 8", got)
	}
}

// TestHitTest maps known points to item indices: the centre of a pill hits it,
// the title row hits nothing, the pill's own top-left corner hits it, and a
// point beyond all pills hits nothing.
func TestHitTest(t *testing.T) {
	items, links := mkItems([]string{"AA", "BB", "CC"}, []string{"DD", "EE"})
	gl := layout(twoColTestWidth, items, links)

	// Centre of item 1 (links row 0, col 1).
	p1 := gl.placements[1].rect
	if got := gl.hitTest(p1.X+p1.W/2, p1.Y); got != 1 {
		t.Errorf("hitTest centre of item 1 = %d, want 1", got)
	}

	// The very top-left cell is the title row — no item there.
	if got := gl.hitTest(0, 0); got != -1 {
		t.Errorf("hitTest(0,0) = %d, want -1 (title row)", got)
	}

	// Top-left corner of item 0's pill should hit item 0.
	p0 := gl.placements[0].rect
	if got := gl.hitTest(p0.X, p0.Y); got != 0 {
		t.Errorf("hitTest top-left of item 0 = %d, want 0", got)
	}

	// A point far to the right, beyond all pills, hits nothing.
	if got := gl.hitTest(twoColTestWidth+50, p0.Y); got != -1 {
		t.Errorf("hitTest off-grid = %d, want -1", got)
	}
}
