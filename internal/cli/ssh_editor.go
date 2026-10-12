package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/editorlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/execstream"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	"github.com/BenjaminBenetti/fleet-man/internal/sshserver"
	"github.com/spf13/cobra"
)

// ssh_editor.go is VS Code Remote-SSH's way into an instance. ~/.ssh/fleet_config
// (written by `fleet code` and the TUI's `c`) makes OpenSSH run
// `fleet ssh-proxy %n` for every *.fleet host. ssh-proxy reads the alias,
// finds the daemon it names and runs `fleet ssh-server --stdio` in the
// instance over that daemon's Exec stream, with OpenSSH's connection on its
// stdio. Both commands are plumbing, hence hidden.

// newSSHProxyCmd creates the hidden `fleet ssh-proxy <alias>` command.
func newSSHProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:          sshalias.ProxySubcommand + " <alias>",
		Short:        "Carry an ssh connection to an instance's ssh-server (ProxyCommand; internal)",
		Hidden:       true,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			code, err := runSSHProxy(ctx, args[0], os.Stdin, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
}

// sshProxyResolve finds the daemon an alias leads to, and the armada remote
// URL it is reached by ("" for none), a var for tests.
var sshProxyResolve = func(ctx context.Context, alias string) (sshalias.Target, editorlaunch.Dialer, string, error) {
	return editorlaunch.ResolveAliasRemote(ctx, alias, editorlaunch.LocalDaemon)
}

// runSSHProxy carries one ssh connection (in, out) to the ssh-server of the
// instance alias names, returning ssh-server's exit code.
func runSSHProxy(ctx context.Context, alias string, in io.Reader, out, errOut io.Writer) (int, error) {
	target, dial, remoteURL, err := sshProxyResolve(ctx, alias)
	if err != nil {
		return 0, err
	}
	conn, err := dial(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return sshProxyOver(ctx, conn.Service(), target, remoteURL, in, out, errOut)
}

// sshProxyOver runs the instance's ssh-server over svc with the connection on
// its stdio. Through an armada remote with [ agent: on ] this machine's
// ssh-agent is provided for the connection's life, as a remote `fleet
// shell` provides it: the editor's git and ssh in the instance use the
// user's keys.
func sshProxyOver(ctx context.Context, svc fleetgrpc.FleetServiceClient, target sshalias.Target, remoteURL string, in io.Reader, out, errOut io.Writer) (int, error) {
	stopAgent := forwardAgentTo(ctx, svc, remoteURL, true)
	defer stopAgent()
	code, err := execstream.Run(ctx, svc, execstream.Options{
		Fleet:    target.Fleet,
		Instance: target.Instance,
		Argv:     sshalias.ServerArgv(),
		Stdin:    in,
		Stdout:   out,
		Stderr:   errOut,
	})
	if err != nil {
		return code, fmt.Errorf("%s/%s: %w", target.Fleet, target.Instance, err)
	}
	return code, nil
}

// newSSHServerCmd creates the hidden `fleet ssh-server --stdio` command: an
// SSH server for the one connection on its stdin and stdout, inside an
// instance. It has no listen mode.
func newSSHServerCmd() *cobra.Command {
	var stdio bool
	cmd := &cobra.Command{
		Use:          sshalias.ServerSubcommand + " --stdio",
		Short:        "Serve one SSH connection on stdio inside an instance (internal)",
		Hidden:       true,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !stdio {
				return errors.New("ssh-server only serves the connection on its stdio: run it with --stdio")
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// stdout is the connection: reports go to stderr, which the
			// daemon also copies into the instance's creation log.
			return sshserver.Serve(ctx, os.Stdin, os.Stdout, sshserver.Options{Log: os.Stderr})
		},
	}
	cmd.Flags().BoolVar(&stdio, "stdio", false, "serve the connection on stdin and stdout")
	return cmd
}
