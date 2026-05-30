package tui

import (
	"fmt"
	"os/exec"
	"strings"
)

// openInTerminal spawns a new terminal window running the given command.
// Tries common Linux terminal emulators in order of preference.
func openInTerminal(command []string) error {
	// Try common Linux terminal emulators in order
	terminals := []struct {
		bin  string
		args func([]string) []string
	}{
		{"ptyxis", func(cmd []string) []string {
			return append([]string{"--"}, cmd...)
		}},
		{"gnome-terminal", func(cmd []string) []string {
			return append([]string{"--"}, cmd...)
		}},
		{"konsole", func(cmd []string) []string {
			return append([]string{"-e"}, cmd...)
		}},
		{"xfce4-terminal", func(cmd []string) []string {
			return append([]string{"-e"}, cmd...)
		}},
		{"alacritty", func(cmd []string) []string {
			return append([]string{"-e"}, cmd...)
		}},
		{"kitty", func(cmd []string) []string {
			return cmd
		}},
		{"xterm", func(cmd []string) []string {
			return append([]string{"-e"}, cmd...)
		}},
	}

	tried := make([]string, 0, len(terminals))
	for _, term := range terminals {
		tried = append(tried, term.bin)
		if _, err := exec.LookPath(term.bin); err == nil {
			args := term.args(command)
			return exec.Command(term.bin, args...).Start()
		}
	}

	return fmt.Errorf("no terminal found (tried: %s)", strings.Join(tried, ", "))
}
