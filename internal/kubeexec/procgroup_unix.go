//go:build !nok8s && unix

package kubeexec

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs cmd in a process group of its own and makes a cancel
// kill the whole group: a plugin is often a script or a CLI that forks, and
// killing only the direct child would leave the rest running.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
