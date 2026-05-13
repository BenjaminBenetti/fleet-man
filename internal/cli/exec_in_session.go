package cli

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
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
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			sessionName := args[1]
			command := strings.Join(args[2:], " ")

			st, err := state.Load()
			if err != nil {
				return err
			}

			f, ok := st.Fleets[target.Fleet]
			if !ok {
				return fmt.Errorf("fleet %q not found", target.Fleet)
			}

			instance, err := f.GetInstance(target.Instance)
			if err != nil {
				return err
			}

			if instance.Status != fleet.StatusRunning {
				return fmt.Errorf("instance %s is not running (status: %s)", args[0], instance.Status)
			}

			instanceBackend := backendutil.NewForInstance(instance, false)

			// Send the command to the tmux session
			// We use tmux send-keys to send the command followed by Enter
			execCmd := instanceBackend.ExecCommand(instance.WorkspaceDir, []string{
				"sh", "-c",
				fmt.Sprintf(`tmux send-keys -t %s %s Enter`, dotfiles.ShQuote(sessionName), dotfiles.ShQuote(command)),
			})

			if err := execCmd.Run(); err != nil {
				return fmt.Errorf("failed to execute command in session %q: %w", sessionName, err)
			}

			fmt.Printf("Executed command in session %q of instance %s\n", sessionName, args[0])
			return nil
		},
	}
}
