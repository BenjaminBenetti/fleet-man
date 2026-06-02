package cli

import (
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// sessionExecCommand is the seam used by spawn-session, exec-in-session,
// and read-session to dispatch a shell command at an instance's backend.
// Tests override this so commands run against a private tmux server on the
// host instead of being shelled into a real container.
var sessionExecCommand = func(instance *fleet.Instance, args []string) *exec.Cmd {
	// Unwrapping to the raw *exec.Cmd via .Cmd bypasses the wrapper's single
	// timed "container exec" log. That is intentional: spawn-session and
	// exec-in-session log their own timed events ("session created" /
	// "session exec" with ms), so a generic "container exec" would
	// double-count the same work; read-session is a plain screen read. The
	// test seam also returns a plain *exec.Cmd, so this layer matches it.
	return backendutil.NewForInstance(instance, false).ExecCommand(instance.WorkspaceDir, args).Cmd
}
