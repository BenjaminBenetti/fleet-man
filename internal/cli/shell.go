package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func newShellCmd() *cobra.Command {
	var groupFlag string
	var sessionFlag string

	cmd := &cobra.Command{
		Use:   "shell <name> [-- command...]",
		Short: "Open a persistent shell inside an instance",
		Long: `Opens a tmux-backed shell inside a devcontainer instance.

By default, creates a new session group. Use --group to add a pane to an
existing group, or --session to reconnect to a specific named session.

A command after -- replaces the default tmux shell: it is run interactively
inside the instance exactly as given (--group/--session are ignored). The TUI
uses this to attach panes through a remote daemon.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			var shellCmd []string
			if len(args) > 1 {
				// Explicit command mode: the caller (e.g. the TUI's remote
				// attach) already built the exact in-instance argv, including
				// any session handling — run it as-is.
				shellCmd = args[1:]
			} else {
				// Pre-server local read of config (the configutil carve-out) to honor
				// the tmux-vim-keys preference when building the in-container shell.
				config, _ := configutil.LoadConfig()
				nested := os.Getenv("TMUX") != ""

				sanitized := tui.SanitizeSessionName(target.Instance)
				var sessionName string
				switch {
				case sessionFlag != "":
					// Reconnect to a specific existing session. Short names are
					// canonicalized so `--session foo` reaches the same session
					// `spawn-session <inst> foo` created.
					sessionName = tui.ResolveSessionName(target.Instance, sessionFlag)
				case groupFlag != "":
					// Add a new pane to an existing group.
					sessionName = tui.NewGroupPaneSessionName(sanitized, groupFlag)
				default:
					// Create a new group (root session).
					sessionName = tui.NewGroupRootSessionName(sanitized)
				}

				// Tag the outer tmux pane with the session name so the TUI can read
				// pane titles to preserve pane order when saving/restoring group
				// layouts. Target this specific pane via TMUX_PANE.
				if nested {
					if paneID := os.Getenv("TMUX_PANE"); paneID != "" {
						_ = exec.Command("tmux", "select-pane", "-t", paneID, "-T", sessionName).Run()
					} else {
						_ = exec.Command("tmux", "select-pane", "-T", sessionName).Run()
					}
				}

				cols, rows := termSize()
				shellCmd = tui.ShellCommandForSession(config, sessionName, cols, rows, nested)
			}

			if fleetclient.IsRemote() {
				// Remote daemon: the resolved argv would reference binaries and
				// containers on the daemon's host, so stream the terminal over
				// the Exec bidi RPC instead of exec'ing locally.
				return runRemoteShell(cmd.Context(), target.Fleet, target.Instance, shellCmd)
			}

			// The server resolves the backend exec argv; we run it locally so the
			// user's terminal is inherited (the TTY carve-out — no direct backend).
			argv, env, err := resolveExecArgv(cmd.Context(), target.Fleet, target.Instance, shellCmd)
			if err != nil {
				return err
			}
			if len(argv) == 0 {
				return fmt.Errorf("no shell command resolved for %s/%s", target.Fleet, target.Instance)
			}
			execCmd := exec.Command(argv[0], argv[1:]...)
			execCmd.Env = mergeExecEnv(env)
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
