package theme

import "github.com/charmbracelet/lipgloss"

// c is a terse constructor so the palettes below read as color tables.
func c(v string) lipgloss.Color { return lipgloss.Color(v) }

// Fleet is the original look: the ANSI-256 indices the TUI was hardcoded with
// before themes existed, reproduced exactly (bit-for-bit the same escape
// sequences), and no tmux chrome of its own. It is the default.
func Fleet() Theme {
	return Theme{
		Name: Default,
		Dark: true,

		Accent:  c("170"),
		Primary: c("39"),
		Border:  c("63"),

		Success: c("42"),
		Warning: c("214"),
		Error:   c("196"),
		ErrorBg: c("52"),
		Danger:  c("203"),
		Purple:  c("99"),
		Info:    c("44"),

		Muted:     c("240"),
		Subtle:    c("241"),
		Faint:     c("238"),
		Collapsed: c("245"),
		Text:      c("252"),
		Message:   c("229"),

		// Light cyan to deep blue.
		GradientFrom: RGB{130, 220, 255},
		GradientTo:   RGB{60, 80, 200},

		Instance: InstancePalette{
			Red: c("196"), Orange: c("214"), Yellow: c("226"), Green: c("42"),
			Cyan: c("39"), Blue: c("69"), Purple: c("170"), Pink: c("213"),
		},
	}
}

// GruvboxDark — https://github.com/morhetz/gruvbox (dark, medium contrast).
func GruvboxDark() Theme {
	return Theme{
		Name: "Gruvbox Dark",
		Dark: true,

		Accent:  c("#d3869b"), // bright purple
		Primary: c("#83a598"), // bright blue
		Border:  c("#7c6f64"), // bg4

		Success: c("#b8bb26"), // bright green
		Warning: c("#fe8019"), // bright orange
		Error:   c("#fb4934"), // bright red
		ErrorBg: c("#9d0006"), // faded red
		Danger:  c("#fe8019"),
		Purple:  c("#b16286"), // neutral purple
		Info:    c("#8ec07c"), // bright aqua

		Muted:     c("#928374"), // gray
		Subtle:    c("#928374"),
		Faint:     c("#504945"), // bg2
		Collapsed: c("#a89984"), // fg4
		Text:      c("#ebdbb2"), // fg1
		Message:   c("#fabd2f"), // bright yellow

		GradientFrom: RGB{250, 189, 47}, // yellow
		GradientTo:   RGB{214, 93, 14},  // faded orange

		Instance: InstancePalette{
			Red: c("#fb4934"), Orange: c("#fe8019"), Yellow: c("#fabd2f"), Green: c("#b8bb26"),
			Cyan: c("#8ec07c"), Blue: c("#83a598"), Purple: c("#b16286"), Pink: c("#d3869b"),
		},

		Pane: PaneChrome{Border: "fg=#504945", ActiveBorder: "fg=#fabd2f"},
	}
}

// GruvboxLight — gruvbox light, medium contrast.
func GruvboxLight() Theme {
	return Theme{
		Name: "Gruvbox Light",
		Dark: false,

		Accent:  c("#8f3f71"), // purple
		Primary: c("#076678"), // blue
		Border:  c("#a89984"), // bg4

		Success: c("#79740e"), // green
		Warning: c("#af3a03"), // orange
		Error:   c("#9d0006"), // red
		ErrorBg: c("#f2c8bb"), // pale red tint (not a gruvbox stop; the banner needs a light bg)
		Danger:  c("#cc241d"), // neutral red
		Purple:  c("#b16286"), // neutral purple
		Info:    c("#427b58"), // aqua

		Muted:     c("#928374"), // gray
		Subtle:    c("#7c6f64"), // fg4
		Faint:     c("#d5c4a1"), // bg2
		Collapsed: c("#7c6f64"),
		Text:      c("#3c3836"), // fg1
		Message:   c("#b57614"), // yellow

		GradientFrom: RGB{215, 153, 33}, // neutral yellow
		GradientTo:   RGB{175, 58, 3},   // orange

		Instance: InstancePalette{
			Red: c("#9d0006"), Orange: c("#af3a03"), Yellow: c("#b57614"), Green: c("#79740e"),
			Cyan: c("#427b58"), Blue: c("#076678"), Purple: c("#8f3f71"), Pink: c("#b16286"),
		},

		Pane: PaneChrome{Border: "fg=#d5c4a1", ActiveBorder: "fg=#b57614"},
	}
}

// CatppuccinMocha — https://github.com/catppuccin/catppuccin (Mocha).
func CatppuccinMocha() Theme {
	return Theme{
		Name: "Catppuccin Mocha",
		Dark: true,

		Accent:  c("#cba6f7"), // mauve
		Primary: c("#89b4fa"), // blue
		Border:  c("#6c7086"), // overlay0

		Success: c("#a6e3a1"), // green
		Warning: c("#fab387"), // peach
		Error:   c("#f38ba8"), // red
		ErrorBg: c("#5b2a3c"), // red-tinted surface (not a catppuccin stop)
		Danger:  c("#eba0ac"), // maroon
		Purple:  c("#b4befe"), // lavender
		Info:    c("#94e2d5"), // teal

		Muted:     c("#6c7086"), // overlay0
		Subtle:    c("#7f849c"), // overlay1
		Faint:     c("#45475a"), // surface1
		Collapsed: c("#9399b2"), // overlay2
		Text:      c("#cdd6f4"), // text
		Message:   c("#f9e2af"), // yellow

		GradientFrom: RGB{137, 220, 235}, // sky
		GradientTo:   RGB{203, 166, 247}, // mauve

		Instance: InstancePalette{
			Red: c("#f38ba8"), Orange: c("#fab387"), Yellow: c("#f9e2af"), Green: c("#a6e3a1"),
			Cyan: c("#94e2d5"), Blue: c("#89b4fa"), Purple: c("#cba6f7"), Pink: c("#f5c2e7"),
		},

		Pane: PaneChrome{Border: "fg=#45475a", ActiveBorder: "fg=#cba6f7"},
	}
}

// CatppuccinLatte — catppuccin Latte, the light flavour.
func CatppuccinLatte() Theme {
	return Theme{
		Name: "Catppuccin Latte",
		Dark: false,

		Accent:  c("#8839ef"), // mauve
		Primary: c("#1e66f5"), // blue
		Border:  c("#9ca0b0"), // overlay0

		Success: c("#40a02b"), // green
		Warning: c("#fe640b"), // peach
		Error:   c("#d20f39"), // red
		ErrorBg: c("#f4c7d0"), // pale red tint (not a catppuccin stop)
		Danger:  c("#e64553"), // maroon
		Purple:  c("#7287fd"), // lavender
		Info:    c("#179299"), // teal

		Muted:     c("#9ca0b0"), // overlay0
		Subtle:    c("#8c8fa1"), // overlay1
		Faint:     c("#ccd0da"), // surface0
		Collapsed: c("#7c7f93"), // overlay2
		Text:      c("#4c4f69"), // text
		Message:   c("#df8e1d"), // yellow

		GradientFrom: RGB{4, 165, 229},  // sky
		GradientTo:   RGB{136, 57, 239}, // mauve

		Instance: InstancePalette{
			Red: c("#d20f39"), Orange: c("#fe640b"), Yellow: c("#df8e1d"), Green: c("#40a02b"),
			Cyan: c("#179299"), Blue: c("#1e66f5"), Purple: c("#8839ef"), Pink: c("#ea76cb"),
		},

		Pane: PaneChrome{Border: "fg=#ccd0da", ActiveBorder: "fg=#8839ef"},
	}
}

// TokyoNight — https://github.com/folke/tokyonight.nvim (night).
func TokyoNight() Theme {
	return Theme{
		Name: "Tokyo Night",
		Dark: true,

		Accent:  c("#bb9af7"), // magenta
		Primary: c("#7aa2f7"), // blue
		Border:  c("#545c7e"), // dark3

		Success: c("#9ece6a"), // green
		Warning: c("#ff9e64"), // orange
		Error:   c("#f7768e"), // red
		ErrorBg: c("#4b2a35"), // red-tinted surface (not a tokyonight stop)
		Danger:  c("#db4b4b"), // red1
		Purple:  c("#9d7cd8"), // purple
		Info:    c("#7dcfff"), // cyan

		Muted:     c("#565f89"), // comment
		Subtle:    c("#737aa2"), // dark5
		Faint:     c("#292e42"), // bg_highlight
		Collapsed: c("#a9b1d6"), // fg_dark
		Text:      c("#c0caf5"), // fg
		Message:   c("#e0af68"), // yellow

		GradientFrom: RGB{125, 207, 255}, // cyan
		GradientTo:   RGB{187, 154, 247}, // magenta

		Instance: InstancePalette{
			Red: c("#f7768e"), Orange: c("#ff9e64"), Yellow: c("#e0af68"), Green: c("#9ece6a"),
			Cyan: c("#7dcfff"), Blue: c("#7aa2f7"), Purple: c("#bb9af7"), Pink: c("#ff007c"),
		},

		Pane: PaneChrome{Border: "fg=#292e42", ActiveBorder: "fg=#7aa2f7"},
	}
}

// TokyoNightDay — tokyonight's light variant.
func TokyoNightDay() Theme {
	return Theme{
		Name: "Tokyo Night Day",
		Dark: false,

		Accent:  c("#9854f1"), // magenta
		Primary: c("#2e7de9"), // blue
		Border:  c("#8990b3"), // dark3

		Success: c("#587539"), // green
		Warning: c("#b15c00"), // orange
		Error:   c("#f52a65"), // red
		ErrorBg: c("#f5c9d6"), // pale red tint (not a tokyonight stop)
		Danger:  c("#c64343"), // red1
		Purple:  c("#7847bd"), // purple
		Info:    c("#007197"), // cyan

		Muted:     c("#848cb5"), // comment
		Subtle:    c("#6172b0"), // dark5
		Faint:     c("#c4c8da"), // bg_highlight
		Collapsed: c("#6172b0"),
		Text:      c("#3760bf"), // fg
		Message:   c("#8c6c3e"), // yellow

		GradientFrom: RGB{0, 113, 151},  // cyan
		GradientTo:   RGB{152, 84, 241}, // magenta

		Instance: InstancePalette{
			Red: c("#f52a65"), Orange: c("#b15c00"), Yellow: c("#8c6c3e"), Green: c("#587539"),
			Cyan: c("#007197"), Blue: c("#2e7de9"), Purple: c("#9854f1"), Pink: c("#d20065"),
		},

		Pane: PaneChrome{Border: "fg=#c4c8da", ActiveBorder: "fg=#2e7de9"},
	}
}

// SolarizedLight — https://ethanschoonover.com/solarized/ (light).
func SolarizedLight() Theme {
	return Theme{
		Name: "Solarized Light",
		Dark: false,

		Accent:  c("#d33682"), // magenta
		Primary: c("#268bd2"), // blue
		Border:  c("#93a1a1"), // base1

		Success: c("#859900"), // green
		Warning: c("#cb4b16"), // orange
		Error:   c("#dc322f"), // red
		ErrorBg: c("#f5d0cc"), // pale red tint (not a solarized stop)
		Danger:  c("#cb4b16"),
		Purple:  c("#6c71c4"), // violet
		Info:    c("#2aa198"), // cyan

		Muted:     c("#93a1a1"), // base1
		Subtle:    c("#839496"), // base0
		Faint:     c("#eee8d5"), // base2
		Collapsed: c("#839496"),
		Text:      c("#657b83"), // base00
		Message:   c("#b58900"), // yellow

		GradientFrom: RGB{42, 161, 152}, // cyan
		GradientTo:   RGB{38, 139, 210}, // blue

		Instance: InstancePalette{
			Red: c("#dc322f"), Orange: c("#cb4b16"), Yellow: c("#b58900"), Green: c("#859900"),
			Cyan: c("#2aa198"), Blue: c("#268bd2"), Purple: c("#6c71c4"), Pink: c("#d33682"),
		},

		Pane: PaneChrome{Border: "fg=#eee8d5", ActiveBorder: "fg=#268bd2"},
	}
}
