package launchtui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ===========================================
// Rendering
// ===========================================
//
// The stylesheet deliberately mirrors the host TUI's palette (see
// internal/tui/styles.go) so the launcher feels like part of the same app:
// colour 170 (magenta/pink) is the primary accent and selection, 39 (cyan) is
// the section-header/secondary colour, and 241 is the dim help text. Pills are
// drawn as plain coloured text whose colour alternates by global render index
// between pink and cyan so adjacent options are easy to tell apart; only the
// selected pill gets a solid magenta box drawn behind it.

var (
	// headerStyle renders a section header ("Links" / "Apps") in the secondary
	// cyan the host TUI uses for fleet/section headers.
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			MarginLeft(horizontalMargin)

	// dividerStyle renders the horizontal rule beneath a section header in the
	// same cyan as the header, separating the label from its pills.
	dividerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			MarginLeft(horizontalMargin)

	// statusStyle renders the bottom status line in the host TUI's dim help
	// colour.
	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginLeft(horizontalMargin)

	// degradedStyle renders the status line when no host connection exists, in
	// the host TUI's error red.
	degradedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			MarginLeft(horizontalMargin)

	// pillTextEven / pillTextOdd are the two alternating text colours for
	// unselected pills — the app's two accents, pink and cyan — so neighbouring
	// options are easy to tell apart with no background fill.
	pillTextEven = lipgloss.Color("170") // pink
	pillTextOdd  = lipgloss.Color("39")  // cyan/blue

	// pillSelectedBg / pillSelectedFg style the focused pill as a solid box in
	// the magenta selection accent the host TUI uses, with dark text so it pops
	// against the surrounding plain-text pills.
	pillSelectedBg = lipgloss.Color("170")
	pillSelectedFg = lipgloss.Color("16")
)

// View satisfies tea.Model. It renders the two labelled sections of pills and
// the status/help line. Composition uses lipgloss's ANSI-aware
// JoinHorizontal/JoinVertical rather than a hand-rolled character grid: the
// pills are already styled (their background fills carry escape sequences), and
// only lipgloss's join primitives measure and stack such strings without
// shredding those sequences.
//
// The vertical stack is built to match layout()'s geometry exactly so the
// mouse handler's hit-testing stays in sync: for each section a one-line label
// and a one-line divider (headerRows = 2) followed by its wrapped rows of pills
// (stacked with no gap between rows). Empty sections contribute no pill lines —
// which is also what layout() assumes when it offsets the Apps section by the
// (possibly zero) number of Link rows.
func (m model) View() string {
	if m.width == 0 {
		// No size yet (first frame before WindowSizeMsg); render nothing to
		// avoid a misplaced flash.
		return ""
	}

	gl := m.grid()
	divider := m.divider()

	// "Links" label + divider (headerRows), then its pills.
	parts := []string{
		headerStyle.Render("Links"),
		divider,
	}
	if links := m.renderSection(gl, sectionLink); links != "" {
		parts = append(parts, links)
	}
	// "Apps" label + divider (headerRows), then its pills.
	parts = append(parts, headerStyle.Render("Apps"), divider)
	if apps := m.renderSection(gl, sectionApp); apps != "" {
		parts = append(parts, apps)
	}

	body := lipgloss.JoinVertical(lipgloss.Left, parts...)
	return body + "\n" + m.footer()
}

// renderSection renders one section's pills as a left-indented, wrapping grid:
// each row's pills are joined horizontally (separated by pillGap blank columns)
// and the rows are stacked with no blank line between them (rowStride = 1), so
// the output matches layout()'s geometry exactly. Pills are drawn in flattened
// (global) index order so the alternating fill reads as one continuous pattern
// across both sections. Returns "" when the section has no items, so View can
// omit it and keep the vertical geometry aligned with layout().
func (m model) renderSection(gl gridLayout, sec section) string {
	// Group this section's item indices by their layout row.
	byRow := map[int][]int{}
	maxRow := -1
	for i, p := range gl.placements {
		if p.section != sec {
			continue
		}
		byRow[p.row] = append(byRow[p.row], i)
		if p.row > maxRow {
			maxRow = p.row
		}
	}
	if maxRow < 0 {
		return ""
	}

	gap := strings.Repeat(" ", pillGap)
	indent := strings.Repeat(" ", horizontalMargin)
	var lines []string
	for row := 0; row <= maxRow; row++ {
		var pills []string
		for n, i := range byRow[row] {
			if n > 0 {
				pills = append(pills, gap)
			}
			pills = append(pills, m.renderPill(gl.placements[i], i))
		}
		lines = append(lines, indent+lipgloss.JoinHorizontal(lipgloss.Top, pills...))
	}
	return strings.Join(lines, "\n")
}

// divider renders the horizontal rule shown beneath a section header: a run of
// box-drawing dashes spanning the available content width, in the header's
// cyan, indented to the same left margin as the labels and pills.
func (m model) divider() string {
	width := m.width - 2*horizontalMargin
	if width < 1 {
		width = 1
	}
	return dividerStyle.Render(strings.Repeat("─", width))
}

// renderPill styles the i-th item as a compact single-line pill: just its title,
// no border and no subtitle. Unselected pills are plain coloured text whose
// colour alternates by global index i (pink/cyan) so neighbouring options
// differ; the selected pill instead gets a solid magenta box with dark text.
// Either way the rendered width is the label plus pillPadding on each side
// (padding is whitespace when unselected, the box's interior when selected) —
// exactly the width layout() recorded in the placement's rect, so rendering and
// hit-testing stay in lock-step.
func (m model) renderPill(p itemPlacement, i int) string {
	style := lipgloss.NewStyle().Padding(0, pillPadding)
	switch {
	case i == m.cursor:
		style = style.Background(pillSelectedBg).Foreground(pillSelectedFg).Bold(true)
	case i%2 == 0:
		style = style.Foreground(pillTextEven)
	default:
		style = style.Foreground(pillTextOdd)
	}
	return style.Render(p.label)
}

// footer renders the status line (or the degraded warning) above a one-line key
// hint.
func (m model) footer() string {
	var status string
	switch {
	case m.degrade:
		status = degradedStyle.Render(m.status)
	case m.status != "":
		status = statusStyle.Render(m.status)
	default:
		status = statusStyle.Render(" ")
	}
	help := statusStyle.Render("↑/↓/←/→ or hjkl move · enter/click open · q quit")
	return status + "\n" + help
}

// ===========================================
// Small rendering helpers
// ===========================================

// itemTitle returns the title to show for an item, falling back to the link URL
// (or a generic "App" label) when no explicit title was configured.
func itemTitle(it item) string {
	if it.title != "" {
		return it.title
	}
	if it.kind == kindLink {
		return it.url
	}
	return "App"
}

// localhostHint renders an app's "localhost:<port>" subtitle, or empty when the
// port is unset.
func localhostHint(port int) string {
	if port <= 0 {
		return ""
	}
	return "localhost:" + strconv.Itoa(port)
}

// truncate shortens s to at most max display cells, appending an ellipsis when
// it had to cut. A non-positive max yields the empty string.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}
