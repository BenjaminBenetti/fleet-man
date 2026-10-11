package backendutil

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
)

// TestTodaysBackendsHaveNoClusterCapabilities: the optional capabilities the
// server prefers when a backend has them (StreamExecer, PortDialer with
// HalfCloseDialer and PortWarmer, ControlChannel, WorkspaceOwner,
// RestartAware, fleetlaunch.Stager) are for backends whose instances run out
// of the daemon host's reach. devcontainer,
// codespaces and coder have none of them, so they keep their host-side paths
// (docker exec, the stdio forwarder, the bind-mounted control directory, the
// host checkout, EnsureFresh's own staging). A method added to one of them by
// accident would silently switch those paths.
func TestTodaysBackendsHaveNoClusterCapabilities(t *testing.T) {
	for _, bt := range []fleet.BackendType{fleet.BackendDevcontainer, fleet.BackendCodespaces, fleet.BackendCoder} {
		inst := &fleet.Instance{Name: "i", Backend: bt, WorkspaceDir: "/ws/f/i", ContainerID: "c1"}
		for _, b := range []any{NewForInstance(inst, false), CapabilitiesFor(inst)} {
			if _, ok := b.(backend.StreamExecer); ok {
				t.Errorf("%s: is a StreamExecer", bt)
			}
			if _, ok := b.(backend.PortDialer); ok {
				t.Errorf("%s: is a PortDialer", bt)
			}
			if _, ok := b.(backend.HalfCloseDialer); ok {
				t.Errorf("%s: is a HalfCloseDialer", bt)
			}
			if _, ok := b.(backend.PortWarmer); ok {
				t.Errorf("%s: is a PortWarmer", bt)
			}
			if _, ok := b.(backend.ControlChannel); ok {
				t.Errorf("%s: carries a ControlChannel", bt)
			}
			if _, ok := b.(backend.WorkspaceOwner); ok {
				t.Errorf("%s: is a WorkspaceOwner", bt)
			}
			if _, ok := b.(backend.RestartAware); ok {
				t.Errorf("%s: is RestartAware", bt)
			}
			if _, ok := b.(fleetlaunch.Stager); ok {
				t.Errorf("%s: is a fleetlaunch.Stager", bt)
			}
		}
	}
}
