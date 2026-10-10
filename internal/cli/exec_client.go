package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/execstream"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// exec_client.go reaches a container through the server's ResolveExecCommand RPC
// instead of constructing a backend locally (the P5 boundary): the server
// returns the argv it would exec, and the client runs it locally so the user's
// terminal (TTY) is inherited — the permitted client-host action for interactive
// attach. Reused by exec/shell/session commands.
//
// That local-exec TTY carve-out only holds for a LOCAL daemon. Against a remote
// one (FLEET_GATEWAY / FLEET_SERVER) the resolved argv references binaries and
// containers on the daemon's host, so interactive attach streams the terminal
// over the Exec bidi RPC instead (runRemoteShell → internal/execstream).

// resolveExecArgv asks the server for the argv (+ env) to run argv inside the
// instance; attach says a terminal attaches with it (which clears the
// instance's "sessions reset" notice). A package var so tests can stub the
// whole RPC.
var resolveExecArgv = func(ctx context.Context, fleetName, instanceName string, argv []string, attach bool) ([]string, map[string]string, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	reply, err := conn.Service().ResolveExecCommand(ctx, &fleetgrpc.ResolveExecCommandRequest{
		Fleet:    fleetName,
		Instance: instanceName,
		Argv:     argv,
		Attach:   attach,
	})
	if err != nil {
		return nil, nil, err
	}
	return reply.GetArgv(), reply.GetEnv(), nil
}

// runResolvedExec resolves the exec argv and runs it locally with the caller's
// stdio inherited.
func runResolvedExec(ctx context.Context, fleetName, instanceName string, argv []string) error {
	resolved, env, err := resolveExecArgv(ctx, fleetName, instanceName, argv, false)
	if err != nil {
		return err
	}
	if len(resolved) == 0 {
		return fmt.Errorf("server returned no exec command for %s/%s", fleetName, instanceName)
	}
	c := exec.Command(resolved[0], resolved[1:]...)
	c.Env = mergeExecEnv(env)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// runRemoteShell attaches the caller's terminal to argv inside the instance by
// streaming over the Exec bidi RPC — the remote-daemon counterpart of the TTY
// carve-out. A non-zero remote exit surfaces the same way the local path's
// *exec.ExitError does: as an "exit status N" error (main exits 1 either way).
func runRemoteShell(ctx context.Context, fleetName, instanceName string, argv []string) error {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// git in the remote shell uses the user's keys when the Armada remote
	// forwards the agent. This provider yields to the TUI's (the daemon tries
	// it only after non-yielding providers), so a shell started from the TUI
	// does not displace it.
	stopAgent := forwardAgentWhile(ctx, conn.Service(), true)
	defer stopAgent()
	code, err := execstream.Run(ctx, conn.Service(), execstream.Options{
		Fleet:    fleetName,
		Instance: instanceName,
		Argv:     argv,
		TTY:      true,
		Stdin:    os.Stdin,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	})
	return remoteShellResult(code, err)
}

// remoteShellResult is how `fleet shell` ends after a remote attach ended with
// code (and err, the exit frame's error or the stream's). A pod replaced under
// the attach ends it with exit 75 and a "connection lost" error: the caller's
// wrapper (a TUI pane, a full-screen attach) re-runs it on that code, so the
// shell exits 75 itself rather than main's generic 1.
func remoteShellResult(code int, err error) error {
	if code == kubeRejoinExitCode {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		exitRemoteShell(code)
		return err // reached only when exitRemoteShell is stubbed
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit status %d", code)
	}
	return nil
}

// rejoinLocalExit carries a local attach's exit 75 (the kube-exec shim's
// rejoin signal for a replaced pod) through `fleet shell`, as
// remoteShellResult does for a remote one; any other result is err as is.
func rejoinLocalExit(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == kubeRejoinExitCode {
		exitRemoteShell(kubeRejoinExitCode)
	}
	return err
}

// kubeRejoinExitCode is podwire.ExitHeldStreamGone: a replaced pod, the attach
// wrapper's signal to re-run. exitRemoteShell is os.Exit (a var for tests).
const kubeRejoinExitCode = 75

var exitRemoteShell = os.Exit

// logsStdout is the sink for streamLogs, overridable in tests.
var logsStdout io.Writer = os.Stdout

// streamLogs opens the server's Logs stream and prints each line.
func streamLogs(ctx context.Context, fleetName, instanceName string, follow bool) error {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := conn.Service().Logs(ctx, &fleetgrpc.LogsRequest{
		Fleet:    fleetName,
		Instance: instanceName,
		Follow:   follow,
	})
	if err != nil {
		return err
	}
	for {
		line, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(logsStdout, line.GetLine())
	}
}

// mergeExecEnv layers the server-supplied env over the client's own. Nil/empty
// means "inherit" (a nil Cmd.Env makes exec inherit os.Environ()).
func mergeExecEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
