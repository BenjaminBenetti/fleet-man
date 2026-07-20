package dotfiles

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// ShQuote returns value wrapped in single quotes, safe for a /bin/sh literal.
// Thin delegate kept for the established name; the one implementation lives in
// internal/shellquote.
func ShQuote(value string) string {
	return shellquote.Single(value)
}

// TmuxEnsureInstalled is a shell snippet that installs tmux if it is not
// already present. Shared by the CLI's spawn-session/exec-in-session commands
// and the TUI's session-creation paths so containers without tmux preinstalled
// get it on first use.
//
// `apt-get update` and `apt-get install` are joined with `;` (not `&&`) inside
// a brace group: some images have stale or misconfigured apt repos where
// `update` exits non-zero (e.g. a GPG warning on a third-party repo), which
// would otherwise short-circuit the install. The group's exit code is the
// install's, so the fallback chain still works correctly.
const TmuxEnsureInstalled = `command -v tmux >/dev/null 2>&1 || { echo '==> Installing tmux...'; { apt-get update -qq; apt-get install -y -qq tmux; } 2>/dev/null || { sudo apt-get update -qq; sudo apt-get install -y -qq tmux; } 2>/dev/null || (apk add tmux) 2>/dev/null || (sudo apk add tmux) 2>/dev/null || (dnf install -y tmux) 2>/dev/null || (sudo dnf install -y tmux) 2>/dev/null || echo 'ERROR: failed to install tmux'; }; `

// SetupScript returns the raw shell snippet for dotfiles installation
// regardless of the auto-install setting. Returns empty if dotfiles are not
// configured (repo URL or install script missing).
func SetupScript(config *state.Config) string {
	if config == nil {
		return ""
	}
	repo := config.DotfilesSettings.RepoURL
	script := config.DotfilesSettings.InstallScript
	if repo == "" || script == "" {
		return ""
	}
	// Run the install script under setsid so that any long-lived
	// processes it spawns (e.g. sshfs's ssh subprocess) are placed in a
	// new session/process group.  Without this, those children inherit
	// the docker-exec pty's process group and receive SIGHUP when the
	// user detaches from tmux, killing SSHFS mounts.
	return fmt.Sprintf(
		`if [ ! -d ~/dotfiles ]; then echo '==> Cloning dotfiles...'; GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new' git clone %s ~/dotfiles && (cd ~/dotfiles && setsid sh %s); fi; `,
		ShQuote(repo), ShQuote(script),
	)
}
