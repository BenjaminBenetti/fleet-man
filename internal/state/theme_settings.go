package state

// ThemeSettings holds the TUI color theme (issue #251). It is a CLIENT
// preference: the TUI reads and writes it on the user's LOCAL daemon only
// (like the armada registry), so switching the TUI onto a remote fleet never
// changes its look. The server stores the name verbatim — the theme list lives
// client-side (internal/theme) and an unknown name renders as the default.
type ThemeSettings struct {
	// Name is a built-in theme name ("Fleet", "Gruvbox Dark", …). Empty means
	// the default (Fleet) look.
	Name string `json:"name,omitempty"`
}
