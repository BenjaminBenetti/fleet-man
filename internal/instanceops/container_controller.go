package instanceops

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// containerController is the minimal backend surface the lifecycle
// transitions need: start and stop a container by ID. It is an interface
// (rather than a concrete backend) so tests can swap in a double via
// newClient without standing up a real backend or container.
type containerController interface {
	Start(containerID string) error
	Stop(containerID string) error
}

// newClient builds the containerController for a backend type. It is a
// package-level var so tests can replace it with a constructor that returns
// a double.
var newClient = func(backendType fleet.BackendType) containerController {
	return backendutil.New(backendType, false)
}
