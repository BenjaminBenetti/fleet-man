package deps

import (
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/platform"
)

// Dependency describes a binary dependency and how to install it.
type Dependency struct {
	Name        string
	Binary      string
	Required    bool
	InstallURL  string
	Description string
	Found       bool
}

// Check looks up whether the devcontainer and coder binaries are
// available on the user's PATH and returns a report for each.
func Check() []Dependency {
	deps := []Dependency{
		{
			Name:        "tmux",
			Binary:      "tmux",
			Required:    true,
			InstallURL:  "https://github.com/tmux/tmux/wiki/Installing",
			Description: "Terminal multiplexer — fleet runs its TUI inside a tmux session.",
		},
		{
			Name:       "devcontainer CLI",
			Binary:     "devcontainer",
			Required:   true,
			InstallURL: "https://github.com/devcontainers/cli#npm-install",
		},
		{
			Name:       "Coder CLI",
			Binary:     "coder",
			Required:   false,
			InstallURL: "https://coder.com/docs/install",
		},
	}
	if platform.IsWSL() {
		deps = append(deps, Dependency{
			Name:        "Windows clipboard",
			Binary:      "powershell.exe",
			Required:    false,
			Description: "Copy/paste support through the Windows clipboard when running inside WSL.",
		})
	} else {
		deps = append(deps,
			Dependency{
				Name:        "wl-clipboard",
				Binary:      "wl-copy",
				Required:    false,
				InstallURL:  wlClipboardInstallURL(),
				Description: "Copy/paste support on Wayland (only one of wl-clipboard or xclip needed).",
			},
			Dependency{
				Name:        "xclip",
				Binary:      "xclip",
				Required:    false,
				InstallURL:  "https://github.com/astrand/xclip",
				Description: "Copy/paste support on X11 (only one of wl-copy or xclip needed).",
			},
		)
	}

	for i := range deps {
		_, err := exec.LookPath(deps[i].Binary)
		deps[i].Found = err == nil
	}

	return deps
}

// HasMissing returns true if any dependency in the list was not found.
func HasMissing(deps []Dependency) bool {
	for _, dep := range deps {
		if !dep.Found {
			return true
		}
	}
	return false
}
