package tui

import (
	"github.com/BenjaminBenetti/fleet-man/internal/theme"
	"github.com/charmbracelet/lipgloss"
)

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
// The first entry is the default (no custom styling applied). The NAMES are
// what an instance stores (its color is portable across themes); the colors
// behind them come from the active theme's palette — applyTheme rebuilds the
// list, so "purple" under Gruvbox is Gruvbox's purple.
var instanceColors = buildInstanceColors(theme.Fleet().Instance)

// buildInstanceColors maps the fixed color names onto a theme's palette.
func buildInstanceColors(p theme.InstancePalette) []InstanceColorOption {
	return []InstanceColorOption{
		{Name: instanceColorWhite},
		{Name: "red", Color: p.Red},
		{Name: "orange", Color: p.Orange},
		{Name: "yellow", Color: p.Yellow},
		{Name: "green", Color: p.Green},
		{Name: "cyan", Color: p.Cyan},
		{Name: "blue", Color: p.Blue},
		{Name: "purple", Color: p.Purple},
		{Name: "pink", Color: p.Pink},
	}
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
