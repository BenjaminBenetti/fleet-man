package cli

import (
	"context"
	"os/exec"
)

// sessionExecCommand is the seam used by spawn-session, exec-in-session, and
// read-session to dispatch a shell command at an instance's backend. The server
// resolves the backend exec argv (ResolveExecCommand) and the client runs it
// locally — no direct backend access (the P5 boundary). Tests override this so
// commands run against a private tmux server on the host instead of a real
// container.
var sessionExecCommand = func(fleetName, instanceName string, args []string) (*exec.Cmd, error) {
	argv, env, err := resolveExecArgv(context.Background(), fleetName, instanceName, args)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, nil
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = mergeExecEnv(env)
	return c, nil
}
