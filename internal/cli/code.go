package cli

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
	"github.com/spf13/cobra"
)

func newCodeCmd() *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "code <name>",
		Short: "Open VS Code attached to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}

			argv := editorArgv(target.Fleet, instance)
			if printOnly {
				fmt.Fprintln(cmd.OutOrStdout(), commandLine(argv))
				return nil
			}

			fmt.Printf("Opening VS Code for %s/%s...\n", target.Fleet, target.Instance)
			c := exec.Command(argv[0], argv[1:]...)
			switch instance.Backend {
			case fleet.BackendCoder, fleet.BackendCodespaces:
				return runEditorCmd(c)
			default:
				return c.Run()
			}
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the editor command instead of running it")
	return cmd
}

// editorArgv is the command that opens VS Code on instance, a member of
// fleetName. Each backend opens VS Code with a local CLI; no container access
// is needed, so this is computed client-side (no internal/backend). The TUI's
// `c` key builds the same command.
func editorArgv(fleetName string, instance *fleet.Instance) []string {
	switch instance.Backend {
	case fleet.BackendCoder:
		return []string{"coder", "open", "vscode", instance.ContainerID}
	case fleet.BackendCodespaces:
		return []string{"gh", "codespace", "code", "-c", instance.ContainerID}
	default:
		// Devcontainer: VS Code's dev-container remote URI (hex of the host
		// workspace path + the in-container /workspaces/<project> folder).
		uri := fmt.Sprintf("vscode-remote://dev-container+%s/workspaces/%s",
			hex.EncodeToString([]byte(instance.WorkspaceDir)), fleetName)
		return []string{"code", "--folder-uri", uri}
	}
}

// commandLine joins argv into one line a POSIX shell runs as the same command,
// quoting only the words that need it.
func commandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, word := range argv {
		words[i] = shellquote.CommandWord(word)
	}
	return strings.Join(words, " ")
}

func runEditorCmd(c *exec.Cmd) error {
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
