package launchtui

import "testing"

// TestMoveLeftRight covers horizontal stepping through the flattened order,
// including clamping at both ends and the empty list.
func TestMoveLeftRight(t *testing.T) {
	tests := []struct {
		name          string
		cursor, count int
		want          int
	}{
		{name: "right from middle", cursor: 2, count: 5, want: 3},
		{name: "right clamps at end", cursor: 4, count: 5, want: 4},
		{name: "right on empty", cursor: 0, count: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moveRight(tt.cursor, tt.count); got != tt.want {
				t.Errorf("moveRight(%d,%d) = %d, want %d", tt.cursor, tt.count, got, tt.want)
			}
		})
	}

	leftTests := []struct {
		name          string
		cursor, count int
		want          int
	}{
		{"left from middle", 2, 5, 1},
		{"left clamps at 0", 0, 5, 0},
		{"left on empty", 0, 0, 0},
	}
	for _, tt := range leftTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moveLeft(tt.cursor, tt.count); got != tt.want {
				t.Errorf("moveLeft(%d,%d) = %d, want %d", tt.cursor, tt.count, got, tt.want)
			}
		})
	}
}

// TestMoveVertical exercises the geometry-driven up/down motion across the
// interesting cases: single row, multi-row wrap, crossing the Links→Apps
// section boundary, and a ragged last row. All titles are width 2 so pills are
// uniform and two fit per row at twoColTestWidth, which makes the expected
// landing indices deterministic.
func TestMoveVertical(t *testing.T) {
	width := twoColTestWidth

	t.Run("single row has nothing below or above", func(t *testing.T) {
		items, links := mkItems([]string{"AA", "BB"}, nil) // one row
		gl := layout(width, items, links)
		if got := moveDown(gl, 0); got != 0 {
			t.Errorf("moveDown from single row = %d, want 0 (unchanged)", got)
		}
		if got := moveUp(gl, 1); got != 1 {
			t.Errorf("moveUp from single row = %d, want 1 (unchanged)", got)
		}
	})

	t.Run("multi-row wrap moves straight down a column", func(t *testing.T) {
		items, links := mkItems([]string{"AA", "BB", "CC", "DD"}, nil) // rows {0:[0,1],1:[2,3]}
		gl := layout(width, items, links)
		if got := moveDown(gl, 0); got != 2 {
			t.Errorf("moveDown from 0 = %d, want 2", got)
		}
		if got := moveDown(gl, 1); got != 3 {
			t.Errorf("moveDown from 1 = %d, want 3", got)
		}
		if got := moveUp(gl, 3); got != 1 {
			t.Errorf("moveUp from 3 = %d, want 1", got)
		}
	})

	t.Run("crossing section boundary lands on nearest app column", func(t *testing.T) {
		// 2 links (single row: cols 0,1), 2 apps (single row: cols 0,1).
		items, links := mkItems([]string{"AA", "BB"}, []string{"DD", "EE"})
		gl := layout(width, items, links)
		// Down from link col 1 (index 1) should land on app col 1 (index 3).
		if got := moveDown(gl, 1); got != 3 {
			t.Errorf("moveDown across boundary from 1 = %d, want 3", got)
		}
		// Down from link col 0 (index 0) lands on app col 0 (index 2).
		if got := moveDown(gl, 0); got != 2 {
			t.Errorf("moveDown across boundary from 0 = %d, want 2", got)
		}
		// Up from app col 1 (index 3) returns to link col 1 (index 1).
		if got := moveUp(gl, 3); got != 1 {
			t.Errorf("moveUp across boundary from 3 = %d, want 1", got)
		}
	})

	t.Run("ragged last row clamps to nearest column", func(t *testing.T) {
		// 3 links: rows {0:[0,1], 1:[2]}. From link index 1 (row0 col1),
		// moving down should land on the only item in row 1 (index 2, col 0).
		items, links := mkItems([]string{"AA", "BB", "CC"}, nil)
		gl := layout(width, items, links)
		if got := moveDown(gl, 1); got != 2 {
			t.Errorf("moveDown into ragged row from 1 = %d, want 2", got)
		}
		// Up from the lone ragged item (index 2, col 0) returns to col 0 above
		// (index 0).
		if got := moveUp(gl, 2); got != 0 {
			t.Errorf("moveUp out of ragged row from 2 = %d, want 0", got)
		}
	})
}
