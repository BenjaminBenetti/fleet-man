package cli

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/spf13/cobra"
)

func newSpawnSessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "spawn-session <instance> <session-name>",
		Short: "Spawn a new tmux session inside an instance",
		Long: `Creates a new detached tmux session with the specified name inside a devcontainer instance.

The session is created in detached mode, making it ideal for AI agents to spawn
shell sessions programmatically without interactive attachment.

Examples:
  fleet spawn-session agent-1 my-session
  fleet spawn-session my-fleet/agent-1 task-session`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			sessionName := args[1]

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

			// Create a detached tmux session with the specified name.
			createCmd := instanceBackend.ExecCommand(instance.WorkspaceDir, []string{
				"sh", "-c",
				dotfiles.TmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s`, dotfiles.ShQuote(sessionName)),
			})

			if err := createCmd.Run(); err != nil {
				return fmt.Errorf("failed to create session %q: %w", sessionName, err)
			}

			fmt.Printf("Created tmux session %q in instance %s\n", sessionName, args[0])
			return nil
		},
	}
}
