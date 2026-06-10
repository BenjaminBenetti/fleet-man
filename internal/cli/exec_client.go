package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// exec_client.go reaches a container through the server's ResolveExecCommand RPC
// instead of constructing a backend locally (the P5 boundary): the server
// returns the argv it would exec, and the client runs it locally so the user's
// terminal (TTY) is inherited — the permitted client-host action for interactive
// attach. Reused by exec/shell/session commands.

// resolveExecArgv asks the server for the argv (+ env) to run argv inside the
// instance. A package var so tests can stub the whole RPC.
var resolveExecArgv = func(ctx context.Context, fleetName, instanceName string, argv []string) ([]string, map[string]string, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	reply, err := conn.Service().ResolveExecCommand(ctx, &fleetgrpc.ResolveExecCommandRequest{
		Fleet:    fleetName,
		Instance: instanceName,
		Argv:     argv,
	})
	if err != nil {
		return nil, nil, err
	}
	return reply.GetArgv(), reply.GetEnv(), nil
}

// runResolvedExec resolves the exec argv and runs it locally with the caller's
// stdio inherited.
func runResolvedExec(ctx context.Context, fleetName, instanceName string, argv []string) error {
	resolved, env, err := resolveExecArgv(ctx, fleetName, instanceName, argv)
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
