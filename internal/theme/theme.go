// Package theme defines the fleet TUI color themes (issue #251).
//
// A Theme is a set of semantic color slots — accent, primary, border, the
// status colors, the greys — that the TUI builds its lipgloss styles from,
// plus the instance color cycle, the banner gradient and the tmux pane chrome
// that goes with them. The TUI never names a raw color itself: every color it
// draws comes from a slot here, so switching themes is one call.
//
// The package is pure data: it imports nothing server-side and stays usable
// from any client. Colors are lipgloss.Color strings — an ANSI-256 index for
// the built-in Fleet look (which must reproduce the pre-theme rendering
// exactly) and hex truecolor for everything else; lipgloss degrades hex to the
// nearest 256-color entry on a terminal (or a tmux) that lacks truecolor.
package theme

import "github.com/charmbracelet/lipgloss"

// Theme is one complete color scheme.
type Theme struct {
	// Name is the user-visible, stable identifier ("Gruvbox Dark"). It is what
	// the config stores, so renaming a theme orphans saved preferences.
	Name string
	// Dark reports whether the scheme is meant for a dark terminal background.
	// Fleet never paints a page background, so a light theme is a set of
	// accents that read on a light terminal, not a light page.
	Dark bool

	// Accent marks what the user is acting on: titles, the cursor, the selected
	// row, dialog frames and titles, key names, the spinner.
	Accent lipgloss.Color
	// Primary is the scheme's main hue: expanded fleet headers, session rows,
	// the stopped status, the keybindings frame.
	Primary lipgloss.Color
	// Border frames the lists (list box, split divider) and the scrollbar.
	Border lipgloss.Color

	// Success / Warning / Error are the status trio: running / creating /
	// failed, PR checks, agent indicators.
	Success lipgloss.Color
	Warning lipgloss.Color
	Error   lipgloss.Color
	// ErrorBg backs the red warning banner (Error text on top of it).
	ErrorBg lipgloss.Color
	// Danger frames the security prompts (unknown ssh host key, copy/open
	// request from an instance) — attention-grabbing but distinct from a
	// failure.
	Danger lipgloss.Color
	// Purple tags a closed/merged PR (GitHub's own hue for it).
	Purple lipgloss.Color
	// Info is the automation-spawned marker: an origin badge, calmer than a
	// status.
	Info lipgloss.Color

	// Muted is de-emphasised detail (instance details, pending PR states, an
	// idle agent). Subtle is the help bar and dialog hints. Faint is barely
	// there (the scrollbar track). Collapsed is a folded fleet header. Text is
	// body text in dialogs and the keybindings list. Message is the transient
	// status line.
	Muted     lipgloss.Color
	Subtle    lipgloss.Color
	Faint     lipgloss.Color
	Collapsed lipgloss.Color
	Text      lipgloss.Color
	Message   lipgloss.Color

	// GradientFrom / GradientTo are the endpoints of the banner (logo / page
	// title) gradient.
	GradientFrom RGB
	GradientTo   RGB

	// Instance is the palette behind the user-selectable instance colors.
	Instance InstancePalette

	// Pane is the tmux chrome. Empty style strings mean "leave tmux alone",
	// which is what the Fleet theme does (today's look has no chrome of its
	// own — the user's tmux config shows through).
	Pane PaneChrome
}

// RGB is a banner gradient endpoint.
type RGB struct{ R, G, B float64 }

// InstancePalette maps the eight named instance colors (the cycle the user
// steps through with the color keys) to the scheme's take on each.
type InstancePalette struct {
	Red, Orange, Yellow, Green, Cyan, Blue, Purple, Pink lipgloss.Color
}

// PaneChrome holds tmux style strings ("fg=#504945") for the window the TUI
// runs in: Border for every pane divider, ActiveBorder for the divider of the
// focused pane.
type PaneChrome struct {
	Border       string
	ActiveBorder string
}

// Default is the name of the built-in look and the fallback for an unknown or
// empty saved name.
const Default = "Fleet"

// Lookup returns the theme called name, or the Fleet theme when name is empty
// or unknown (a preference saved by a newer fleet, or a typo in config.json,
// must never blank the TUI).
func Lookup(name string) Theme {
	for _, t := range All() {
		if t.Name == name {
			return t
		}
	}
	return Fleet()
}

// Names lists the theme names in cycle order: the dark schemes first, then the
// light ones, Fleet leading.
func Names() []string {
	all := All()
	names := make([]string, 0, len(all))
	for _, t := range all {
		names = append(names, t.Name)
	}
	return names
}

// Next returns the theme name direction steps (typically +1 / -1) after
// current in cycle order, wrapping at both ends. An unknown current counts as
// Fleet.
func Next(current string, direction int) string {
	names := Names()
	idx := 0
	for i, n := range names {
		if n == current {
			idx = i
			break
		}
	}
	n := len(names)
	return names[((idx+direction)%n+n)%n]
}

// Kind is the "dark" / "light" label shown next to a theme's name.
func (t Theme) Kind() string {
	if t.Dark {
		return "dark"
	}
	return "light"
}

// All returns every built-in theme in cycle order (see Names).
func All() []Theme {
	return []Theme{
		Fleet(),
		GruvboxDark(),
		CatppuccinMocha(),
		TokyoNight(),
		GruvboxLight(),
		CatppuccinLatte(),
		TokyoNightDay(),
		SolarizedLight(),
	}
}
