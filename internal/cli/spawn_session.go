package cli

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
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
			target, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			// Canonicalize to the TUI's group naming convention
			// (<instance>~<name>) so the session shows up as a regular
			// session group — a bare-named session would surface as a
			// pseudo-group and splitting it from the TUI would mint a
			// duplicate real group with the same name.
			sessionName := tui.ResolveSessionName(target.Instance, args[1])

			if instance.Status != fleet.StatusRunning {
				return fmt.Errorf("instance %s is not running (status: %s)", args[0], instance.Status)
			}

			// Create a detached tmux session with the specified name.
			createCmd, err := sessionExecCommand(target.Fleet, target.Instance, []string{
				"sh", "-c",
				dotfiles.TmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s`, dotfiles.ShQuote(sessionName)),
			})
			if err != nil {
				return fmt.Errorf("failed to create session %q: %w", sessionName, err)
			}

			if err := createCmd.Run(); err != nil {
				return fmt.Errorf("failed to create session %q: %w", sessionName, err)
			}

			fmt.Printf("Created tmux session %q in instance %s\n", sessionName, args[0])
			return nil
		},
	}
}
