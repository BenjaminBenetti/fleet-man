package create

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// controlMount ensures the per-instance host control directory exists and
// returns the bind mount that exposes it inside the instance at
// control.ContainerMountDir.
//
// The host directory is state.ControlDir(fleetName, instanceName) — a
// per-instance directory under the instance's workspace tree. It is created
// with 0777 permissions for the same cross-UID reason as the agentic mount
// resolver: the process inside the container that connects to the control
// socket may run as a different user than the host fleet that creates the
// socket, so the shared directory must be traversable by both. The control
// socket itself does not exist yet at create/clone time — the host fleet TUI
// later creates it inside this directory, and the instance sees it through the
// bind mount (same kernel, shared mount, exactly like docker.sock).
//
// Returning the mount (rather than appending it directly) keeps this helper
// pure and testable: it only touches the host directory and reports what mount
// the backend should honor, leaving the caller to decide whether the backend
// supports custom mounts at all.
func controlMount(fleetName, instanceName string) (backend.Mount, error) {
	dir := state.ControlDir(fleetName, instanceName)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return backend.Mount{}, fmt.Errorf("create control dir %s: %w", dir, err)
	}
	return backend.Mount{
		LocalPath:     dir,
		ContainerPath: control.ContainerMountDir,
	}, nil
}
