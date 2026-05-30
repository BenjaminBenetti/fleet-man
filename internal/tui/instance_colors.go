package tui

import "github.com/charmbracelet/lipgloss"

// ===========================================
// Instance Colors
// ===========================================

// InstanceColorOption pairs a color name with its lipgloss color value.
type InstanceColorOption struct {
	Name  string
	Color lipgloss.Color
}

// instanceColorWhite is the sentinel default color. An empty color value
// means "render with the terminal's default foreground" (no change).
const instanceColorWhite = "white"

// instanceColors is the ordered cycle list of selectable instance colors.
// The first entry is the default (no custom styling applied).
var instanceColors = []InstanceColorOption{
	{Name: instanceColorWhite},
	{Name: "red", Color: lipgloss.Color("196")},
	{Name: "orange", Color: lipgloss.Color("214")},
	{Name: "yellow", Color: lipgloss.Color("226")},
	{Name: "green", Color: lipgloss.Color("42")},
	{Name: "cyan", Color: lipgloss.Color("39")},
	{Name: "blue", Color: lipgloss.Color("69")},
	{Name: "purple", Color: lipgloss.Color("170")},
	{Name: "pink", Color: lipgloss.Color("213")},
}

// nextInstanceColor returns the next color name in instanceColors starting
// from current and advancing by direction (typically +1 or -1).
func nextInstanceColor(current string, direction int) string {
	if current == "" {
		current = instanceColorWhite
	}
	idx := 0
	for i, colorOption := range instanceColors {
		if colorOption.Name == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(instanceColors)) % len(instanceColors)
	return instanceColors[idx].Name
}

// instanceColorStyle resolves a color name to a lipgloss style. The default
// color (empty or "white") returns an unstyled style so rendering falls
// back to the terminal's default appearance.
func instanceColorStyle(name string) lipgloss.Style {
	if name == "" || name == instanceColorWhite {
		return lipgloss.NewStyle()
	}
	for _, colorOption := range instanceColors {
		if colorOption.Name == name {
			return lipgloss.NewStyle().Foreground(colorOption.Color)
		}
	}
	return lipgloss.NewStyle()
}

// instanceColorHasCustom reports whether the given color name produces a
// non-default rendering.
func instanceColorHasCustom(name string) bool {
	return name != "" && name != instanceColorWhite
}
