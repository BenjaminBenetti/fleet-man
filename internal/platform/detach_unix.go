//go:build unix

package platform

import (
	"os/exec"
	"syscall"
)

// detachFromTerminal starts cmd in its own session, so it has no controlling
// terminal and closing the terminal the caller ran from doesn't SIGHUP a viewer
// the opener left running.
func detachFromTerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
