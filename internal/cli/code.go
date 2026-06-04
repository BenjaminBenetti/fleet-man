package cli

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

func newCodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "code <name>",
		Short: "Open VS Code attached to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			fmt.Printf("Opening VS Code for %s/%s...\n", target.Fleet, target.Instance)

			// Each backend opens VS Code with a local CLI; no container access is
			// needed, so these are computed client-side (no internal/backend). The
			// editor URI/args are pure and small enough to inline rather than add a
			// dedicated RPC.
			switch instance.Backend {
			case fleet.BackendCoder:
				return runEditorCmd(exec.Command("coder", "open", "vscode", instance.ContainerID))
			case fleet.BackendCodespaces:
				return runEditorCmd(exec.Command("gh", "codespace", "code", "-c", instance.ContainerID))
			default:
				// Devcontainer: VS Code's dev-container remote URI (hex of the host
				// workspace path + the in-container /workspaces/<project> folder).
				uri := fmt.Sprintf("vscode-remote://dev-container+%s/workspaces/%s",
					hex.EncodeToString([]byte(instance.WorkspaceDir)), target.Fleet)
				return exec.Command("code", "--folder-uri", uri).Run()
			}
		},
	}
}

func runEditorCmd(c *exec.Cmd) error {
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
