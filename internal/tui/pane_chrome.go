package tui

import (
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/theme"
)

// pane_chrome.go dresses the host tmux window the TUI runs in to match the
// theme (issue #251): the pane divider colors (pane-border-style and
// pane-active-border-style). Those are WINDOW options, so they color every
// divider in fleet's window — which is the point (the split panes are fleet's)
// — but they must not outlive the TUI: the previous window-level values are
// captured on the first apply and put back on quit. The Fleet theme sets no
// chrome (it is the pre-theme look, where the user's tmux config shows
// through), so applying it restores the same way.
//
// Pane CONTENTS are deliberately untouched: what a shell or an agent prints
// follows the terminal emulator's palette, which the user sets to the matching
// scheme themselves.

// paneChromeOptions are the tmux window options the chrome sets, in order.
var paneChromeOptions = []string{"pane-border-style", "pane-active-border-style"}

// paneChromeSaved holds the window-level values the options had before the
// first apply ("" = not set at window level, so inherited), keyed by option.
// nil until the first apply, so restore is a no-op for a TUI that never
// dressed the window.
var paneChromeSaved map[string]string

// tmuxRun is the exec seam for the chrome commands (tests stub it).
var tmuxRun = func(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// applyPaneChrome sets the window's divider styles to chrome, first saving
// the current window-level values (once per process). An empty chrome
// restores them, which is how the Fleet theme leaves tmux alone.
func applyPaneChrome(chrome theme.PaneChrome) {
	if paneChromeSaved == nil {
		paneChromeSaved = make(map[string]string, len(paneChromeOptions))
		for _, opt := range paneChromeOptions {
			// -w without -A: the window's OWN value, not what it inherits from
			// the global option — restoring an inherited value as a window
			// override would pin the user's global setting to this window.
			v, _ := tmuxRun("show-options", "-wv", opt)
			paneChromeSaved[opt] = v
		}
	}
	if chrome == (theme.PaneChrome{}) {
		restorePaneChrome()
		return
	}
	_, _ = tmuxRun("set-option", "-w", "pane-border-style", chrome.Border)
	_, _ = tmuxRun("set-option", "-w", "pane-active-border-style", chrome.ActiveBorder)
}

// restorePaneChrome puts back the window-level values captured by the first
// applyPaneChrome: re-set if there was one, unset (fall back to the global /
// inherited value) if there was not. Safe to call more than once and before
// any apply.
func restorePaneChrome() {
	if paneChromeSaved == nil {
		return
	}
	for _, opt := range paneChromeOptions {
		if prev := paneChromeSaved[opt]; prev != "" {
			_, _ = tmuxRun("set-option", "-w", opt, prev)
		} else {
			_, _ = tmuxRun("set-option", "-wu", opt)
		}
	}
}
