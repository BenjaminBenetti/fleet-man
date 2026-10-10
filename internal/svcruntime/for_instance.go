package svcruntime

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/dockerrt"
)

// docker is the daemon host's docker runtime.
var docker Runtime = dockerrt.Runtime{}

// Docker returns the runtime that runs shared services as containers on the
// daemon host's docker.
func Docker() Runtime { return docker }

// ForInstance returns the runtime that runs inst's fleet's shared services for
// inst, or nil when inst can't use any. A devcontainer (an empty Backend is a
// devcontainer, as everywhere) uses Docker: it runs on that docker and can join
// its networks and bind its sockets. A Codespaces or Coder workspace runs on
// someone else's machine and reaches neither, so it gets nil, as does a backend
// type this build does not know. It does no I/O, so it is safe under the state
// lock.
func ForInstance(inst *fleet.Instance) Runtime {
	switch inst.Backend {
	case "", fleet.BackendDevcontainer:
		return docker
	default:
		return nil
	}
}
