package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/editorlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// codeDial connects `fleet code` to the daemon it works against (a var for
// tests).
var codeDial = func(ctx context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn.Service(), func() { _ = conn.Close() }, nil
}

// codeLocal is the local daemon, for the remote records (a var for tests).
var codeLocal = editorlaunch.LocalDaemon

// codeInteractive reports whether `fleet code` may ask on the terminal (a
// var for tests).
var codeInteractive = func() bool { return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stderr.Fd()) }

func newCodeCmd() *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "code <name>",
		Short: "Open VS Code attached to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			svc, done, err := codeDial(ctx)
			if err != nil {
				return err
			}
			defer done()
			launch, err := editorlaunch.Resolve(ctx, editorlaunch.Services{Target: svc, Local: codeLocal}, target.Fleet, target.Instance)
			if err != nil {
				return err
			}
			if printOnly {
				fmt.Fprintln(cmd.OutOrStdout(), editorlaunch.CommandLine(launch.Argv))
				if launch.NeedsInclude {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: %s resolves once ~/.ssh/config starts with `%s` (fleet code adds it with your consent)\n", launch.Alias, sshalias.IncludeLine)
				}
				return nil
			}
			if launch.RemoteSSH() {
				if launch.NeedsInclude {
					if err := consentToInclude(cmd.InOrStdin(), cmd.ErrOrStderr(), launch); err != nil {
						return err
					}
				}
				if err := editorlaunch.CheckRemoteSSH(launch.Argv[0]); err != nil {
					return err
				}
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Opening VS Code for %s/%s...\n", target.Fleet, target.Instance)
			c := exec.Command(launch.Argv[0], launch.Argv[1:]...)
			switch launch.Mode {
			case fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODER_CLI, fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODESPACES_CLI:
				return runEditorCmd(c)
			default:
				return c.Run()
			}
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the editor command instead of running it")
	return cmd
}

// consentToInclude asks, on a terminal, to add the Include line to
// ~/.ssh/config, and adds it on yes. Without a terminal it changes nothing
// and says what to do by hand.
func consentToInclude(in io.Reader, out io.Writer, launch editorlaunch.Launch) error {
	if !codeInteractive() {
		return fmt.Errorf("~/.ssh/config does not include %s yet.\n%s", sshalias.ConfigFileName, editorlaunch.ManualSteps(launch))
	}
	fmt.Fprintf(out, "Kubernetes and remote instances open via VS Code Remote-SSH.\n"+
		"Fleet will add one line to ~/.ssh/config:\n    %s\n"+
		"(hosts %s route through `fleet %s`)\n"+
		"Extensions from devcontainer.json install on first connect.\n"+
		"Add it and open? [y/N] ", sshalias.IncludeLine, sshalias.HostPattern, sshalias.ProxySubcommand)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
	default:
		return errors.New("cancelled: ~/.ssh/config is unchanged.\n" + editorlaunch.ManualSteps(launch))
	}
	return editorlaunch.AddInclude()
}

func runEditorCmd(c *exec.Cmd) error {
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
