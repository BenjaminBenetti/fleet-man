package cli

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
	"github.com/spf13/cobra"
)

func newExecInSessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec-in-session <instance> <session-name> <command...>",
		Short: "Execute a command in a tmux session's shell",
		Long: `Executes a command in the shell of an existing tmux session inside an instance.

The command is sent as keys to the tmux session, followed by Enter. This allows
AI agents to run commands in persistent tmux sessions programmatically.

The tmux session must already exist. Use 'spawn-session' to create a new session first.

Examples:
  fleet exec-in-session agent-1 my-session "echo hello"
  fleet exec-in-session my-fleet/agent-1 task-session "npm test"`,
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			// Same canonicalization as spawn-session, so the short name an
			// agent created with still targets the right session.
			sessionName := tui.ResolveSessionName(target.Instance, args[1])
			command := strings.Join(args[2:], " ")

			if instance.Status != fleet.StatusRunning {
				return fmt.Errorf("instance %s is not running (status: %s)", args[0], instance.Status)
			}

			// Send the command to the tmux session via send-keys
			// followed by Enter so the shell executes it.
			execCmd, err := sessionExecCommand(target.Fleet, target.Instance, []string{
				"sh", "-c",
				fmt.Sprintf(`tmux send-keys -t %s %s Enter`, dotfiles.ShQuote(sessionName), dotfiles.ShQuote(command)),
			})
			if err != nil {
				return fmt.Errorf("failed to execute command in session %q: %w", sessionName, err)
			}

			if err := execCmd.Run(); err != nil {
				return fmt.Errorf("failed to execute command in session %q: %w", sessionName, err)
			}

			fmt.Printf("Executed command in session %q of instance %s\n", sessionName, args[0])
			return nil
		},
	}
}
