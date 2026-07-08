package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// shQuote returns value wrapped in single quotes with any embedded
// single quotes escaped, safe for a /bin/sh literal. Straight to the
// shellquote leaf (not via dotfiles.ShQuote) so an injection audit of the
// TUI ends at the one implementation.
func shQuote(value string) string {
	return shellquote.Single(value)
}

// SanitizeSessionName replaces characters that are problematic in
// socket filenames with hyphens.
func SanitizeSessionName(name string) string {
	replacer := strings.NewReplacer(".", "-", ":", "-", "/", "-")
	sanitized := replacer.Replace(name)
	if sanitized == "" {
		return "fleet"
	}
	return sanitized
}

// dotfilesSetupScript returns the raw shell snippet for dotfiles installation
// regardless of the auto-install setting. Returns empty if dotfiles are not
// configured (repo URL or install script missing).
func dotfilesSetupScript(config *configutil.Config) string {
	return dotfiles.SetupScript(config)
}

// dotfilesSetup returns a shell snippet that clones and installs dotfiles,
// or an empty string if dotfiles are not configured or auto-install is enabled
// (in which case dotfiles are installed in the background on instance creation).
func dotfilesSetup(config *configutil.Config) string {
	if config != nil && config.DotfilesSettings.AutoInstall {
		return ""
	}
	return dotfilesSetupScript(config)
}

// tmuxEnsureInstalled aliases the shared snippet in the dotfiles package.
// Kept as a package-local name so existing TUI call sites stay terse.
var tmuxEnsureInstalled = dotfiles.TmuxEnsureInstalled

// shellCommand returns the command to run inside a devcontainer with a
// persistent tmux session. The session is named after the instance so
// that reconnecting reattaches to the running session.
// If tmux is not installed in the container it is auto-installed.
//
// tmux new-session -A handles all cases in one command:
//   - No session exists: creates a new one.
//   - Session exists: attaches to it.
//
// Ctrl+Q or Ctrl+O detaches and keeps processes running.
//
// cols/rows are the caller's terminal dimensions. When non-zero, stty
// is used to correct the remote PTY size before tmux starts. This is
// needed for backends like coder ssh that may report incorrect sizes
// (e.g. 128x128).
func shellCommand(config *configutil.Config, instanceName string, cols, rows int, nested bool) []string {
	return ShellCommandForSession(config, SanitizeSessionName(instanceName), cols, rows, nested)
}

// ShellCommandForSession returns the command to run inside a devcontainer
// with a persistent tmux session using the given session name. This allows
// connecting to a specific named session rather than the default one derived
// from the instance name.
func ShellCommandForSession(config *configutil.Config, session string, cols, rows int, nested bool) []string {
	setup := dotfilesSetup(config)
	// coder ssh may report incorrect terminal dimensions (e.g. 128x128).
	// We fix the PTY size with stty before tmux starts and pass -x/-y for
	// new session creation. "window-size latest" tells tmux to always
	// track the most recent client's terminal size, so the window
	// auto-resizes on SIGWINCH (e.g. when the user drags a split divider).
	// We avoid resize-window hooks because they put the window into
	// manual-size mode that prevents dynamic resizing.
	sizefix := ""
	tmuxSize := ""
	resizeHook := ""
	if cols > 0 && rows > 0 {
		sizefix = fmt.Sprintf(`stty cols %d rows %d 2>/dev/null; `, cols, rows)
		tmuxSize = fmt.Sprintf(` -x %d -y %d`, cols, rows-1)
		resizeHook = ` \; set -g aggressive-resize on \; set -g window-size latest`
	}
	// Tabs (issue #168): tmux windows inside the inner session act as
	// per-pane tabs. The status line is the tab bar, rendered across the
	// top of the pane with one entry per window; tmux's default
	// MouseDown1Status binding makes each entry click-to-switch, and the
	// default prefix keys (c / n / p / 0-9) create and cycle tabs.
	//
	// tabToggle hides the bar while a session has a single window and
	// shows it otherwise. It runs once at attach time and again from the
	// window-linked/unlinked hooks on every window create/kill. The hooks
	// and the status options are session-scoped so they never touch a
	// user's own tmux sessions on the container's shared server (or
	// clobber their global hooks); only the window-status formats are
	// global, as window options have no session scope. The session-scoped
	// @fleet_tab_autohide marker distinguishes the modes: nested panes
	// auto-hide (the TUI provides all other UI) while full-screen
	// attaches keep the bar always on because status-right also carries
	// the detach hint there.
	tabToggle := `if -F '#{&&:#{@fleet_tab_autohide},#{==:#{session_windows},1}}' 'set status off' 'set status on'`
	tabBarConf := ` \; set status-position top` +
		` \; set status-style 'bg=default,fg=colour245'` +
		` \; set -g window-status-format ' #I:#W '` +
		` \; set -g window-status-current-format '#[fg=colour39,bold] #I:#W #[default]'` +
		` \; set-hook window-linked "` + tabToggle + `"` +
		` \; set-hook window-unlinked "` + tabToggle + `"`
	// When nested inside a host tmux (split pane mode), use Ctrl+X as
	// the inner prefix so it doesn't conflict with the outer Ctrl+B.
	// Pane navigation (h/j/k/l) is handled by the outer tmux, so the
	// inner tmux only needs prefix and session keys.
	modeConf := ""
	statusRight := ` ctrl+q/ctrl+o: detach `
	if nested {
		// In nested mode, Ctrl+Q/O are handled by the outer tmux
		// (they close all split panes). The inner tmux only needs
		// the prefix override and session navigation. The prefix is
		// session-scoped so full-screen attaches and a user's own
		// sessions on the shared server keep the default Ctrl+B
		// (bind-key tables are inherently global; prefix+C-x =
		// send-prefix is inert under other prefixes). The status bar
		// doubles as the tab bar and is hidden while the session has a
		// single window (the outer tmux provides all other UI).
		modeConf = ` \; set prefix C-x \; bind-key C-x send-prefix` +
			` \; set status-left '' \; set status-right ' ctrl+x c: new tab '` +
			` \; set @fleet_tab_autohide 1 \; ` + tabToggle
		statusRight = ""
	} else {
		// Full-screen mode keeps the bar always on: it carries the
		// detach hint and doubles as the tab bar. -u resets the
		// prefix and status-left a prior nested attach overrode, so
		// the documented full-screen keys (ctrl+b …) hold regardless
		// of attach order.
		modeConf = ` \; set @fleet_tab_autohide 0 \; set status on \; set -u status-left \; set -u prefix`
	}
	// Session navigation: Ctrl+PageUp/Down are handled by the outer
	// tmux to cycle session groups. prefix+T creates a new session
	// inside the container.
	sessionKeys := ` \; bind-key T new-session`
	// Clear any stale resize-window hooks from previous sessions before
	// attaching. The hook puts the window into manual-size mode and
	// prevents dynamic resizing. We run this as a one-off tmux command
	// against the existing server (if any) before exec-ing into it.
	// Stabilise SSH agent forwarding across tmux detach/reattach cycles.
	// Each SSH connection creates a new agent socket, but tmux keeps the
	// old SSH_AUTH_SOCK from the original session. We symlink the current
	// socket to a fixed path and point SSH_AUTH_SOCK there so it survives
	// reconnects.
	sshAgentFix := `if [ -n "$SSH_AUTH_SOCK" ] && [ "$SSH_AUTH_SOCK" != "$HOME/.ssh/ssh_auth_sock" ]; then mkdir -p ~/.ssh && ln -sf "$SSH_AUTH_SOCK" ~/.ssh/ssh_auth_sock; if [ ! -S /run/ssh-agent.sock ]; then ln -sf "$SSH_AUTH_SOCK" /run/ssh-agent.sock; fi; export SSH_AUTH_SOCK="$HOME/.ssh/ssh_auth_sock"; fi; `
	hookClear := fmt.Sprintf(
		`tmux has-session -t %s 2>/dev/null && tmux set-hook -gu client-attached 2>/dev/null; `,
		shQuote(session),
	)
	// In non-nested mode, Ctrl+Q/O detach from the inner tmux session.
	// In nested mode, the outer tmux handles these keys to close all
	// split panes, so we don't bind them here.
	detachKeys := ` \; bind-key -n C-q detach-client \; bind-key -n C-o detach-client`
	if nested {
		detachKeys = ""
	}
	statusConf := ""
	if statusRight != "" {
		statusConf = fmt.Sprintf(` \; set status-right '%s'`, statusRight)
	}
	// OSC 52 clipboard: allows tmux to send copied text to the terminal
	// emulator's system clipboard via escape sequences. Works transparently
	// over SSH and inside containers. terminal-features (tmux 3.2+) tells
	// tmux to add the Ms clipboard capability for all terminal types.
	// It is appended last so a failure on older tmux does not prevent
	// preceding commands from executing.
	clipboardConf := ` \; set -g set-clipboard on`
	// Override MouseDragEnd1Pane to use copy-selection (instead of the
	// default copy-selection-and-cancel) so the scroll position is
	// preserved after copying. The user presses q to exit copy-mode.
	//
	// Also unbind MouseDown2Pane. The outer host tmux handles
	// middle-click paste (reading the host PRIMARY selection and
	// pasting the result), so the inner tmux should do nothing if a
	// mouse event ever reaches it. The default binding
	// (select-pane; send-keys -M) is harmless but defensive-unbinding
	// eliminates any path where the inner tmux could paste its own
	// buffer and masquerade as the outer paste.
	mouseBindings := ` \; bind -T copy-mode MouseDragEnd1Pane send-keys -X copy-selection` +
		` \; bind -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-selection` +
		` \; unbind -n MouseDown2Pane`
	clipboardFeatures := ` \; set -as terminal-features ',*:clipboard'`
	inner := setup + tmuxEnsureInstalled + sizefix + sshAgentFix + hookClear + fmt.Sprintf(
		`exec tmux -u new-session -A -s %s`+tmuxSize+` \; set -g mouse on`+clipboardConf+mouseBindings+detachKeys+tabBarConf+statusConf+modeConf+sessionKeys+resizeHook+clipboardFeatures,
		shQuote(session),
	)
	return []string{"sh", "-c", inner}
}

// freshShellCommand returns the command to run inside a devcontainer
// without tmux. Used by the "open in new terminal" action where a fresh,
// non-persistent session is desired.
func freshShellCommand(config *configutil.Config) []string {
	setup := dotfilesSetup(config)
	if setup == "" {
		return []string{"bash"}
	}
	return []string{"sh", "-c", setup + "exec bash"}
}
