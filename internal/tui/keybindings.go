package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// keybindingEntry represents a single key/description row.
type keybindingEntry struct {
	Key  string
	Desc string
}

// keybindingGroup is a titled group of keybinding entries.
type keybindingGroup struct {
	Title   string
	Entries []keybindingEntry
}

// keybindingsData returns all application keybindings grouped by context.
func keybindingsData() []keybindingGroup {
	return []keybindingGroup{
		{
			Title: "Global",
			Entries: []keybindingEntry{
				{"ctrl+c / ctrl+q", "Quit application"},
			},
		},
		{
			Title: "Fleet List",
			Entries: []keybindingEntry{
				{"j / k", "Navigate up / down"},
				{"space / tab", "Expand / collapse"},
				{"enter / e", "Open shell or settings"},
				{"s", "Start / stop instance"},
				{"a", "Add instance"},
				{"n", "New fleet"},
				{"d", "Delete fleet / instance"},
				{"t", "Tag instance"},
				{"f", "Focus fleet (hide others; q/esc to leave)"},
				{"p", "Port forward"},
				{"c", "Open VS Code"},
				{"C", "Clone instance"},
				{"R", "Rebuild instance"},
				{"o", "Open external terminal"},
				{"L", "View logs"},
				{"→ / l, ← / h", "Select / deselect PR status"},
				{"enter (on PR status)", "Open PR in browser"},
				{"A", "Switch armada (remote fleet)"},
				{"r", "Refresh / rename session"},
			},
		},
		{
			Title: "Settings",
			Entries: []keybindingEntry{
				{"j / k", "Navigate up / down"},
				{"left / right", "Cycle option value"},
				{"enter", "Edit / toggle setting"},
				{"esc / q", "Back to fleet list"},
			},
		},
		{
			Title: "Tmux (inner session)",
			Entries: []keybindingEntry{
				{"ctrl+x", "Prefix key (nested)"},
				{"ctrl+PageUp/Down", "Cycle sessions"},
				{"prefix + T", "New tmux session"},
				{"ctrl+q / ctrl+o", "Detach from session"},
				{"j / k (vim mode)", "Select pane down / up"},
			},
		},
		{
			Title: "Tmux (outer / host)",
			Entries: []keybindingEntry{
				{"ctrl+b", "Prefix key (default)"},
				{"h / l", "Navigate panes left / right"},
			},
		},
		{
			Title: "Dialogs",
			Entries: []keybindingEntry{
				{"enter", "Confirm action"},
				{"q / esc", "Cancel / close"},
				{"j / k", "Navigate dialog rows"},
				{"h / l", "Cycle dialog values"},
				{"y / n", "Yes / no (delete)"},
				{"enter", "Activate selected text field"},
			},
		},
	}
}

// renderKeybindingColumn renders a slice of keybinding groups into a
// single column string.
func renderKeybindingColumn(groups []keybindingGroup) string {
	var b strings.Builder
	for i, group := range groups {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(keybindingSectionStyle.Render(group.Title))
		b.WriteString("\n")
		for _, entry := range group.Entries {
			b.WriteString(keybindingKeyStyle.Render(entry.Key))
			b.WriteString(keybindingDescStyle.Render(entry.Desc))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderKeybindingsDialog builds the content for the keybindings overlay.
func (settingsPage *settingsPage) renderKeybindingsDialog() string {
	var b strings.Builder

	b.WriteString(dialogTitle.Render("Keybindings"))
	b.WriteString("\n\n")

	groups := keybindingsData()

	var totalLines int
	for _, group := range groups {
		totalLines += len(group.Entries) + 2
	}
	half := totalLines / 2
	splitAt := len(groups)
	running := 0
	for i, group := range groups {
		running += len(group.Entries) + 2
		if running >= half {
			splitAt = i + 1
			break
		}
	}

	left := renderKeybindingColumn(groups[:splitAt])
	right := renderKeybindingColumn(groups[splitAt:])

	colWidth := 48
	leftCol := lipgloss.NewStyle().Width(colWidth).Render(left)
	rightCol := lipgloss.NewStyle().Width(colWidth).Render(right)

	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol))

	b.WriteString("\n")
	b.WriteString(dialogHint.Render("[esc] Close"))

	return b.String()
}
