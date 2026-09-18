package sshtunnel

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr asks the kernel to SIGTERM the ssh child if the daemon dies
// without cleaning up (SIGKILL, OOM), so a forward never outlives its owner and
// squats the loopback port for the next daemon. Linux-only; other platforms
// rely on Manager.Close (orderly shutdown) alone.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
