package state

import (
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// FleetDir returns the base directory for fleet state.
func FleetDir() string {
	return filepath.Join(os.Getenv("HOME"), ".fleet")
}

// StatePath returns the path to the state file.
func StatePath() string {
	return filepath.Join(FleetDir(), "state.json")
}

// WorkspacesDir returns the base directory for instance workspace clones.
func WorkspacesDir() string {
	return filepath.Join(FleetDir(), "workspaces")
}

// WarnPath returns the path to the host-side warning file for a single
// instance. The TUI watches this path after StatusRunning and surfaces
// the first line as a banner — a non-existent file simply means
// "no warnings". Producers should use WriteWarn rather than constructing
// the path manually so all warnings end up in the same well-known place.
func WarnPath(fleetName, instanceName string) string {
	return filepath.Join(FleetDir(), "logs", fleetName+"-"+instanceName+".warn")
}

// BuildkitDir returns the per-fleet host directory that holds the shared
// buildkit server's unix socket and build cache. It lives next to (not inside)
// the per-instance workspace clones — like the agentic mount dirs — so it
// survives instance churn and is shared by every instance in the fleet. The
// buildkit container bind-mounts this directory and creates buildkitd.sock
// inside it; instances bind-mount it read-write to reach that socket.
func BuildkitDir(fleetName string) string {
	return filepath.Join(WorkspacesDir(), fleetName, ".buildkit")
}

// DebCacheDir returns the per-fleet host directory that holds the shared deb
// package cache (the apt-cacher-ng container's on-disk cache). Like BuildkitDir
// it lives next to the per-instance workspace clones so it survives instance
// churn, is shared by every instance in the fleet, and persists across fleet
// teardown (warming the next fleet of the same name).
func DebCacheDir(fleetName string) string {
	return filepath.Join(WorkspacesDir(), fleetName, ".aptcache")
}

// ImageCacheDir returns the per-fleet host directory that holds the shared
// docker image cache (the registry pull-through container's on-disk storage).
// Same lifecycle/sharing rationale as BuildkitDir and DebCacheDir.
func ImageCacheDir(fleetName string) string {
	return filepath.Join(WorkspacesDir(), fleetName, ".imgcache")
}

// ControlDir returns the host directory bind-mounted into an instance to
// carry the control socket. It is per-instance (not per-fleet) so the host
// can tell which instance a received message came from, and it lives under
// the instance's workspace tree so it shares that tree's lifecycle (created
// when the instance is and cleaned up with it).
func ControlDir(fleetName, instanceName string) string {
	return filepath.Join(WorkspacesDir(), fleetName, instanceName, ".control")
}

// ControlSocketPath returns the host path of an instance's control socket.
// The basename comes from control.SocketName so the host listener and the
// in-container client agree on the same file through the bind mount.
func ControlSocketPath(fleetName, instanceName string) string {
	return filepath.Join(ControlDir(fleetName, instanceName), control.SocketName)
}

// WriteWarn writes warning to the instance's WarnPath. Errors are
// intentionally swallowed: warning files are best-effort surfacing of
// non-fatal failures during instance creation, and a write failure here
// must not itself fail the creation flow. Callers can assume the
// function returns immediately and never panics.
func WriteWarn(fleetName, instanceName, warning string) {
	_ = os.WriteFile(WarnPath(fleetName, instanceName), []byte(warning), 0644)
}
