//go:build unix

package mic

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// lifelineScript wraps a recorder. Before exec'ing it ("$@"), it leaves a
// watchdog behind in the same process group: a subshell blocked reading fd 3,
// the read end of a pipe whose ONLY write end is held by fleet. When that write
// end closes — fleet called release, or fleet DIED, by any means at all — the
// read returns EOF and the watchdog SIGKILLs its own process group (`kill 0`),
// recorder included. The watchdog's own stdio goes to /dev/null so it never
// holds the recorder's stdout pipe open.
const lifelineScript = `( cat <&3 >/dev/null 2>&1; kill -9 0 ) >/dev/null 2>&1 &
exec "$@"`

// guardedCommand builds the command that runs argv as a recorder whose lifetime
// is tied to fleet's, in both directions. The promise is that the microphone
// closes the moment nothing needs it, and a recorder has two ways of escaping:
//
//   - It is often not fleet's direct child. A FLEET_MIC_CAPTURE override runs
//     under `sh -c`, and as soon as that is a script file or a pipeline the
//     shell forks: the process holding the microphone is a GRANDCHILD, and
//     exec.CommandContext's default cancel signals only the direct child. So
//     the recorder gets its own process group (Setpgid) and the whole group is
//     killed — by the watchdog below, from the inside.
//   - But a private group is also cut off from the terminal's: the SIGHUP the
//     kernel sends when a terminal closes no longer reaches the recorder, and
//     cancel never runs if fleet dies of a signal it does not handle (SIGHUP,
//     SIGKILL) or panics. Enumerating signals cannot close that hole; the
//     lifeline does, because a dead process holds no pipes. (A simple recorder
//     dies of SIGPIPE on its own once fleet is gone — parec does — but a script
//     looping over `head` shrugs EPIPE off, and that is exactly the documented
//     override shape.)
//
// release closes fleet's end of the lifeline; call it once the command has
// finished (it is what lets the watchdog go when the recorder exits by itself).
func guardedCommand(ctx context.Context, argv ...string) (cmd *exec.Cmd, release func(), err error) {
	lifelineR, lifelineW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	// argv[0] of the wrapper shell is a label; the recorder's argv follows as "$@".
	args := append([]string{"-c", lifelineScript, "fleet-mic"}, argv...)
	cmd = exec.CommandContext(ctx, "sh", args...)
	cmd.ExtraFiles = []*os.File{lifelineR} // fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = lifelineR.Close()
			_ = lifelineW.Close()
		})
	}
	cmd.Cancel = func() error {
		// Cancel IS "cut the lifeline": the watchdog — a member of the group —
		// kills the group. Deliberately not syscall.Kill(-pgid) from here: a
		// cancel racing the recorder's own exit can run after Wait has reaped
		// it, when the pgid may already belong to someone else. A signal sent
		// from inside the group cannot miss. Process.Kill (which the stdlib
		// guards against exactly that reuse) then covers the direct child in
		// case the watchdog is somehow gone.
		release()
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	return cmd, release, nil
}
