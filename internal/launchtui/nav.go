package launchtui

// ===========================================
// Pure navigation
// ===========================================
//
// Cursor movement is split out as pure functions over a gridLayout so the
// keyboard handling in Update stays a thin dispatch and the tricky cases —
// wrapping rows, a ragged last row, crossing the Links→Apps section boundary —
// are exercised by table tests without spinning up a live Bubble Tea program.
//
// The cursor is an index into the flattened links-then-apps item list. Left and
// right step through that flat order (so they naturally flow across row and
// section boundaries). Up and down move by visual geometry: they find the item
// whose square sits directly above/below the current one, which keeps vertical
// motion intuitive even when the two sections have different row widths or the
// last row is ragged.

// moveLeft returns the cursor one item earlier in the flattened order, clamped
// at the first item. With no items it returns 0.
func moveLeft(cursor, count int) int {
	if count == 0 {
		return 0
	}
	if cursor <= 0 {
		return 0
	}
	return cursor - 1
}

// moveRight returns the cursor one item later in the flattened order, clamped
// at the last item. With no items it returns 0.
func moveRight(cursor, count int) int {
	if count == 0 {
		return 0
	}
	if cursor >= count-1 {
		return count - 1
	}
	return cursor + 1
}

// moveDown returns the index of the item visually below the cursor. It picks
// the placement whose pill starts on the next row down and whose horizontal
// centre is nearest the current pill's centre — so a move from the last
// (ragged) Links row lands on the closest Apps pill, and a move within a section
// steps to the pill most directly beneath. When nothing sits below (the cursor
// is already on the bottom row of the last section) the cursor is returned
// unchanged.
func moveDown(gl gridLayout, cursor int) int {
	return moveVertical(gl, cursor, +1)
}

// moveUp returns the index of the item visually above the cursor, mirroring
// moveDown. When nothing sits above (the cursor is on the very top row) the
// cursor is returned unchanged.
func moveUp(gl gridLayout, cursor int) int {
	return moveVertical(gl, cursor, -1)
}

// moveVertical is the shared engine for moveUp/moveDown. dir is +1 for down and
// -1 for up. It scans for the candidate row of pills immediately adjacent to the
// cursor's row (by rectangle Y), then within that row picks the pill whose
// horizontal centre is closest to the cursor's pill centre. Closeness by pixel
// centre (rather than column index) is what keeps vertical motion intuitive now
// that pills are variable width — column 2 of one row need not sit beneath
// column 2 of the next. Working in Y also handles the gap that section headers
// insert between the last Links row and the first Apps row.
func moveVertical(gl gridLayout, cursor, dir int) int {
	if len(gl.placements) == 0 {
		return cursor
	}
	if cursor < 0 || cursor >= len(gl.placements) {
		return cursor
	}
	cur := gl.placements[cursor]
	curCenter := cur.rect.X + cur.rect.W/2

	// Find the nearest adjacent row in the travel direction: the smallest Y
	// strictly greater than the current row (down), or the largest Y strictly
	// less (up).
	targetY := -1
	for _, p := range gl.placements {
		if dir > 0 {
			if p.rect.Y > cur.rect.Y && (targetY == -1 || p.rect.Y < targetY) {
				targetY = p.rect.Y
			}
		} else {
			if p.rect.Y < cur.rect.Y && (targetY == -1 || p.rect.Y > targetY) {
				targetY = p.rect.Y
			}
		}
	}
	if targetY == -1 {
		// No row in that direction; stay put.
		return cursor
	}

	// Among items on the target row, choose the one whose horizontal centre is
	// closest to the current pill's centre. Ties resolve to the earlier
	// (lower-index) item.
	best := cursor
	bestDelta := -1
	for i, p := range gl.placements {
		if p.rect.Y != targetY {
			continue
		}
		delta := abs(p.rect.X + p.rect.W/2 - curCenter)
		if bestDelta == -1 || delta < bestDelta {
			bestDelta = delta
			best = i
		}
	}
	return best
}

// abs returns the absolute value of n.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
