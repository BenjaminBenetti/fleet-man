package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

func newCodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "code <name>",
		Short: "Open VS Code attached to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, _, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			instanceBackend := backendutil.NewForInstance(instance, false)

			// For coder backend, use `coder open vscode` directly
			if instance.Backend == fleet.BackendCoder {
				fmt.Printf("Opening VS Code for %s/%s...\n", target.Fleet, target.Instance)
				coderCmd := exec.Command("coder", backendutil.CoderOpenVSCodeArgs(instance.ContainerID)...)
				coderCmd.Stdout = os.Stdout
				coderCmd.Stderr = os.Stderr
				return coderCmd.Run()
			}

			// For codespaces backend, use `gh codespace code` directly
			if instance.Backend == fleet.BackendCodespaces {
				fmt.Printf("Opening VS Code for %s/%s...\n", target.Fleet, target.Instance)
				ghCmd := exec.Command("gh", "codespace", "code", "-c", instance.ContainerID)
				ghCmd.Stdout = os.Stdout
				ghCmd.Stderr = os.Stderr
				return ghCmd.Run()
			}

			uri, ok := instanceBackend.EditorURI(instance.WorkspaceDir, target.Fleet)
			if !ok {
				return fmt.Errorf("editor integration not supported by this backend")
			}

			fmt.Printf("Opening VS Code for %s/%s...\n", target.Fleet, target.Instance)
			vscode := exec.Command("code", "--folder-uri", uri)
			return vscode.Run()
		},
	}
}
