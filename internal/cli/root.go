package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/doctor"
	"github.com/BenjaminBenetti/fleet-man/internal/platform"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
	"github.com/spf13/cobra"
)

// NewRootCmd creates the root fleet command.
func NewRootCmd() *cobra.Command {
	var runDoctor bool

	root := &cobra.Command{
		Use:   "fleet",
		Short: "Manage fleets of devcontainers",
		Long:  "fleet-man is a CLI/TUI tool for spawning and managing fleets of devcontainers from a repo.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runDoctor {
				return doctor.Run()
			}
			return relaunchInTmux()
		},
	}

	root.Flags().BoolVar(&runDoctor, "doctor", false, "launch a coding agent to diagnose and fix your setup")

	root.AddCommand(
		newUpCmd(),
		newStopCmd(),
		newStartCmd(),
		newDownCmd(),
		newDestroyCmd(),
		newListCmd(),
		newExecCmd(),
		newCodeCmd(),
		newLogsCmd(),
		newStatusCmd(),
		newCreateInstanceCmd(),
		newCloneCmd(),
		newCloneInstanceCmd(),
		newPortForwardCmd(),
		newShellCmd(),
		newSpawnSessionCmd(),
		newExecInSessionCmd(),
	)

	return root
}

// relaunchInTmux re-execs the fleet binary inside a new tmux session.
// If already inside tmux, it runs the TUI directly.
func relaunchInTmux() error {
	if os.Getenv("TMUX") != "" {
		// Already in tmux — just run the TUI.
		return tui.Run()
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}

	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux is required but was not found on PATH. Install it via your package manager (apt/dnf/brew install tmux) and re-run fleet")
	}

	// exec into: tmux new-session -s fleet -- <self>
	// This replaces the current process so the user gets a clean
	// tmux session running fleet with split pane mode enabled.
	// Generate a unique session name so multiple fleet instances can coexist.
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	session := "fleet-" + hex.EncodeToString(suffix[:])

	// Create a DETACHED session first, then configure the server, then
	// attach. terminal-features and set-clipboard must be set before the
	// client connects (attach) so the terminal's Ms clipboard capability
	// is detected at attach time. The detached session keeps the server
	// alive between the two tmux invocations. terminal-features (tmux
	// 3.2+) is last so older versions just lose clipboard, not everything.
	setupArgs := []string{
		"tmux", "new-session", "-d", "-s", session, self,
		";", "set", "-g", "set-clipboard", "on",
		";", "set", "-g", "mouse", "on",
	}

	config, _ := state.LoadConfig()
	if config == nil || config.GeneralSettings.TmuxVimKeysEnabled() {
		setupArgs = append(setupArgs,
			";", "bind-key", "h", "if", "-F", "#{pane_at_left}", "", "select-pane -L",
			";", "bind-key", "l", "if", "-F", "#{pane_at_right}", "", "select-pane -R",
			";", "bind-key", "j", "if", "-F", "#{pane_at_bottom}", "", "select-pane -D",
			";", "bind-key", "k", "if", "-F", "#{pane_at_top}", "", "select-pane -U",
		)
	}
	// Override MouseDragEnd1Pane to use copy-selection so the
	// view stays at the scroll position after copying instead of
	// jumping back to the bottom (the default copy-selection-and-cancel
	// exits copy-mode which resets the scroll).
	setupArgs = append(setupArgs,
		";", "bind", "-T", "copy-mode", "MouseDragEnd1Pane",
		"send-keys", "-X", "copy-selection",
		";", "bind", "-T", "copy-mode-vi", "MouseDragEnd1Pane",
		"send-keys", "-X", "copy-selection",
	)
	// Middle-click paste: `mouse on` makes terminals forward MMB
	// to tmux as an escape sequence instead of doing local X11
	// PRIMARY paste. The outer (host) tmux has access to
	// clipboard tools, so we intercept MouseDown2Pane here, read
	// the host selection or clipboard, and feed it into the clicked
	// pane as bracketed paste. The text flows transparently
	// through any inner container tmux and into the app. Missing
	// clipboard tool or empty clipboard is a silent no-op. On WSL,
	// Windows PowerShell reads the host clipboard. Elsewhere, xsel
	// is tried before xclip because xclip has a stdout-close bug
	// that can stall run-shell; xclip is wrapped in timeout as a
	// belt-and-braces guard.
	//
	// The buffer is given a unique name (__fleet_primary) and
	// deleted after paste. Without this, paste-buffer picks "the
	// most recent buffer" — which can race with OSC 52 writes
	// from the inner tmux (sent whenever the user drag-selects
	// inside a container pane, because set-clipboard on makes
	// tmux accept OSC 52 from its pane contents). The race would
	// cause middle-click to paste the inner tmux's last drag
	// selection instead of the host PRIMARY a fraction of the
	// time.
	setupArgs = append(setupArgs,
		";", "bind-key", "-n", "MouseDown2Pane",
		"run-shell",
		mousePasteCommand(),
	)
	// terminal-features tells tmux the terminal supports OSC 52
	// clipboard and RGB/truecolor (tmux 3.2+). Appended last so
	// older versions just lose these features without breaking the
	// session.
	setupArgs = append(setupArgs,
		";", "set", "-as", "terminal-features", ",*:clipboard:RGB",
	)
	//nolint:errcheck // terminal-features may fail on tmux <3.2
	exec.Command(setupArgs[0], setupArgs[1:]...).Run()

	// Attach: the client connects with terminal-features already set,
	// so Ms (clipboard) is available from the start.
	return syscall.Exec(tmuxBin, []string{
		"tmux", "attach", "-t", session,
	}, os.Environ())
}

func mousePasteCommand() string {
	return `tmux select-pane -t "$TMUX_PANE"; ` +
		`CLIP=$(` + pasteClipboardCommand() + `); ` +
		`[ -n "$CLIP" ] && { tmux set-buffer -b __fleet_primary -- "$CLIP"; tmux paste-buffer -p -d -b __fleet_primary -t "$TMUX_PANE"; }`
}

func pasteClipboardCommand() string {
	return pasteClipboardCommandFor(platform.IsWSL())
}

func pasteClipboardCommandFor(isWSL bool) string {
	if isWSL {
		return `powershell.exe -NoProfile -Command Get-Clipboard 2>/dev/null`
	}
	return `wl-paste -p 2>/dev/null || xsel -op 2>/dev/null || timeout 0.5 xclip -o -selection primary 2>/dev/null || pbpaste 2>/dev/null`
}
