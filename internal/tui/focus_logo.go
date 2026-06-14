package tui

import "strings"

// ===========================================
// Focus-mode Logo
// ===========================================
//
// In focus mode the tall 4-line "fleet" ASCII art is replaced with a compact
// word "Fleet" drawn from Unicode quadrant block characters. Each glyph is
// authored as a small pixel bitmap (6 rows tall) and every 2x2 block of pixels
// collapses to a single quadrant character, so the rendered word is only 3 text
// rows tall — far shorter than the standard banner while still reading as a
// bold, blocky logo. Glyphs are kept an even number of pixels wide and joined
// with an even gap so each letter aligns to the 2x2 quadrant grid and renders
// crisply on its own.

// quadrantRunes maps a 2x2 pixel cell to its Unicode quadrant block glyph. The
// index packs the four pixels as bits: top-left=1, top-right=2, bottom-left=4,
// bottom-right=8.
var quadrantRunes = [16]rune{
	' ', // 0000
	'▘', // 0001 TL
	'▝', // 0010 TR
	'▀', // 0011 TL+TR
	'▖', // 0100 BL
	'▌', // 0101 TL+BL
	'▞', // 0110 TR+BL
	'▛', // 0111 TL+TR+BL
	'▗', // 1000 BR
	'▚', // 1001 TL+BR
	'▐', // 1010 TR+BR
	'▜', // 1011 TL+TR+BR
	'▄', // 1100 BL+BR
	'▙', // 1101 TL+BL+BR
	'▟', // 1110 TR+BL+BR
	'█', // 1111 all
}

// quadrantArt collapses a pixel bitmap (rows of '#' = on, anything else = off)
// into half-height text using quadrant block glyphs. The bitmap is padded to an
// even width and height so every cell has a full 2x2 footprint.
func quadrantArt(bitmap []string) string {
	width := 0
	for _, line := range bitmap {
		if len(line) > width {
			width = len(line)
		}
	}
	if width == 0 {
		return ""
	}
	if width%2 != 0 {
		width++
	}

	rows := make([]string, 0, len(bitmap)+1)
	for _, line := range bitmap {
		if len(line) < width {
			line += strings.Repeat(" ", width-len(line))
		}
		rows = append(rows, line)
	}
	if len(rows)%2 != 0 {
		rows = append(rows, strings.Repeat(" ", width))
	}

	on := func(y, x int) int {
		if rows[y][x] == '#' {
			return 1
		}
		return 0
	}

	var b strings.Builder
	for y := 0; y < len(rows); y += 2 {
		if y > 0 {
			b.WriteByte('\n')
		}
		for x := 0; x < width; x += 2 {
			idx := on(y, x) | on(y, x+1)<<1 | on(y+1, x)<<2 | on(y+1, x+1)<<3
			b.WriteRune(quadrantRunes[idx])
		}
	}
	return b.String()
}

// focusGlyphs holds the 6-row pixel bitmaps for each letter of "Fleet". Every
// glyph is an even number of pixels wide so it aligns to the quadrant grid when
// joined. The capital F and the ascenders l/t reach the top row; the lowercase
// e sits on the x-height (rows 1-5).
var focusGlyphs = map[rune][]string{
	'F': {
		"####",
		"#   ",
		"### ",
		"#   ",
		"#   ",
		"#   ",
	},
	'l': {
		"##",
		"##",
		"##",
		"##",
		"##",
		"##",
	},
	'e': {
		"      ",
		" #### ",
		"##  ##",
		"######",
		"##    ",
		" #### ",
	},
	't': {
		" #  ",
		" #  ",
		"####",
		" #  ",
		" #  ",
		" ## ",
	},
}

// focusWord is the simplified logo text shown in focus mode.
const focusWord = "Fleet"

// focusLogo returns the compact "Fleet" block logo used in focus mode.
func focusLogo() string {
	const rows = 6
	cols := make([]string, rows)
	for i, ch := range focusWord {
		glyph, ok := focusGlyphs[ch]
		if !ok {
			continue
		}
		gap := "  " // even gap keeps the next glyph on the quadrant grid
		if i == 0 {
			gap = ""
		}
		for r := range rows {
			line := ""
			if r < len(glyph) {
				line = glyph[r]
			}
			cols[r] += gap + line
		}
	}
	return quadrantArt(cols)
}
