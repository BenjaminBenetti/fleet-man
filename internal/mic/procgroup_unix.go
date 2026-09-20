//go:build unix

package mic

import (
	"os/exec"
	"syscall"
)

// killWholeGroup makes cancelling cmd kill its entire process group, not just
// the process fleet started.
//
// The recorder is frequently NOT fleet's direct child: a FLEET_MIC_CAPTURE
// override runs under `sh -c`, and as soon as that is a script file, a pipeline
// or more than one command, the shell forks and the process actually holding
// the microphone is a grandchild. exec.CommandContext's default cancel signals
// only the direct child — so the shell would die, Stop would return, the status
// would read "idle", and the recorder would carry on with the microphone open,
// reparented to init, one more per recording. For a feature whose promise is
// that the microphone closes the moment the recorder detaches, killing the
// group is the only correct cancel.
func killWholeGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid = the process group (whose id is the child's pid, per
		// Setpgid). Fall back to the single process if the group is gone.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
