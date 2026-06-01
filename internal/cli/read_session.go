package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/spf13/cobra"
)

// Output sinks for read-session, overridable in tests.
var (
	readSessionStdout io.Writer = os.Stdout
	readSessionStderr io.Writer = os.Stderr
)

func newReadSessionCmd() *cobra.Command {
	var scrollback int

	cmd := &cobra.Command{
		Use:   "read-session <instance> <session-name>",
		Short: "Print the current screen contents of a tmux session",
		Long: `Prints the contents of an existing tmux session to stdout.

Examples:
  fleet read-session agent-1 my-session
  fleet read-session my-fleet/agent-1 task-session --scrollback 200`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, _, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			sessionName := args[1]

			if instance.Status != fleet.StatusRunning {
				return fmt.Errorf("instance %s is not running (status: %s)", args[0], instance.Status)
			}

			// Translate --scrollback into a tmux -S start-line argument.
			// tmux's -S accepts negative numbers (lines into history) or a
			// literal `-` meaning the top of the history buffer.
			startFlag := ""
			switch {
			case scrollback < 0:
				startFlag = "-S - "
			case scrollback > 0:
				startFlag = fmt.Sprintf("-S -%d ", scrollback)
			}

			// Stream tmux's capture directly to our stdout so the caller
			// (typically an AI agent) sees the raw pane contents with no
			// wrapping noise from this process.
			readCmd := []string{
				"sh", "-c",
				fmt.Sprintf(`tmux capture-pane -p %s-t %s`, startFlag, dotfiles.ShQuote(sessionName)),
			}
			captureCmd := sessionExecCommand(instance, readCmd)
			captureCmd.Stdout = readSessionStdout
			captureCmd.Stderr = readSessionStderr

			start := time.Now()
			err = captureCmd.Run()
			if err != nil {
				flog.Error("session read failed", "instance", args[0], "session", sessionName, "cmd", strings.Join(readCmd, " "), "ms", flog.MillisSince(start), "err", err)
				return fmt.Errorf("failed to read session %q: %w", sessionName, err)
			}
			flog.Info("session read", "instance", args[0], "session", sessionName, "cmd", strings.Join(readCmd, " "), "ms", flog.MillisSince(start))

			return nil
		},
	}

	cmd.Flags().IntVarP(&scrollback, "scrollback", "s", 0,
		"include scrollback: 0=visible only, N=last N lines, -1=full history")

	return cmd
}
