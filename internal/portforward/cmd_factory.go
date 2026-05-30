package portforward

import "os/exec"

// CmdFactory builds an unstarted *exec.Cmd for a port forward.
type CmdFactory func(containerID string, localPort, remotePort int) *exec.Cmd
