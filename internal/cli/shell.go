package cli

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func newShellCmd() *cobra.Command {
	var groupFlag string
	var sessionFlag string

	cmd := &cobra.Command{
		Use:   "shell <name>",
		Short: "Open a persistent shell inside an instance",
		Long: `Opens a tmux-backed shell inside a devcontainer instance.

By default, creates a new session group. Use --group to add a pane to an
existing group, or --session to reconnect to a specific named session.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, _, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			config, _ := state.LoadConfig()
			nested := os.Getenv("TMUX") != ""

			sanitized := tui.SanitizeSessionName(instance.Name)
			var sessionName string
			switch {
			case sessionFlag != "":
				// Reconnect to a specific existing session.
				sessionName = sessionFlag
			case groupFlag != "":
				// Add a new pane to an existing group.
				var suffix [2]byte
				_, _ = rand.Read(suffix[:])
				sessionName = sanitized + "~" + groupFlag + "~" + hex.EncodeToString(suffix[:])
			default:
				// Create a new group (root session).
				var suffix [3]byte
				_, _ = rand.Read(suffix[:])
				sessionName = sanitized + "~" + hex.EncodeToString(suffix[:])
			}

			// Tag the outer tmux pane with the session name so the
			// TUI can read pane titles to preserve pane order when
			// saving and restoring group layouts. Use TMUX_PANE to
			// target this specific pane — without -t, select-pane
			// targets whichever pane has focus, which breaks restore
			// when multiple panes are respawned concurrently.
			if nested {
				if paneID := os.Getenv("TMUX_PANE"); paneID != "" {
					_ = exec.Command("tmux", "select-pane", "-t", paneID, "-T", sessionName).Run()
				} else {
					_ = exec.Command("tmux", "select-pane", "-T", sessionName).Run()
				}
			}

			cols, rows := termSize()

			shellCmd := tui.ShellCommandForSession(config, sessionName, cols, rows, nested)
			instanceBackend := backendutil.NewForInstance(instance, false)
			execCmd := instanceBackend.ExecCommand(instance.WorkspaceDir, shellCmd)
			execCmd.Stdin = os.Stdin
			execCmd.Stdout = os.Stdout
			execCmd.Stderr = os.Stderr
			return execCmd.Run()
		},
	}

	cmd.Flags().StringVar(&groupFlag, "group", "", "group ID to add a pane to")
	cmd.Flags().StringVar(&sessionFlag, "session", "", "reconnect to a specific session name")

	return cmd
}

// termSize returns the current terminal dimensions, or (0, 0) on failure.
func termSize() (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}
