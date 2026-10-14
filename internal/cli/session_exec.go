package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// sessionExecCommand is the seam used by spawn-session, exec-in-session, and
// read-session to dispatch a shell command at an instance's backend. The server
// resolves the backend exec argv (ResolveExecCommand) and the client runs it
// locally — no direct backend access (the P5 boundary). Tests override this so
// commands run against a private tmux server on the host instead of a real
// container.
var sessionExecCommand = func(fleetName, instanceName string, args []string) (*exec.Cmd, error) {
	argv, env, err := resolveExecArgv(context.Background(), fleetName, instanceName, args, false)
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

// maxSessionStderr bounds what of a failed session command's stderr its
// error carries.
const maxSessionStderr = 4 << 10

// runSessionCommand runs c and, when it fails, says why in the words of what
// it printed on stderr: tmux's own complaint ("duplicate session: x"), or the
// backend's reason it could not reach the instance at all
// ("kubernetes:unreachable: ..."), ahead of the bare exit status.
func runSessionCommand(c *exec.Cmd) error {
	if c == nil {
		return fmt.Errorf("the instance has no command to run it with")
	}
	var stderr sessionStderr
	c.Stderr = &stderr
	err := c.Run()
	if err == nil {
		return nil
	}
	if why := strings.TrimSpace(string(stderr)); why != "" {
		return fmt.Errorf("%s (%w)", why, err)
	}
	return err
}

// sessionStderr keeps the first maxSessionStderr bytes written to it.
type sessionStderr []byte

func (s *sessionStderr) Write(p []byte) (int, error) {
	if room := maxSessionStderr - len(*s); room > 0 {
		*s = append(*s, p[:min(len(p), room)]...)
	}
	return len(p), nil
}
