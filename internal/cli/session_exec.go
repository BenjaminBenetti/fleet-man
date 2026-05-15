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
	return backendutil.NewForInstance(instance, false).ExecCommand(instance.WorkspaceDir, args)
}
