package backend

// Mount describes a host directory bind-mounted into a container at a
// specified target path. Backends that report SupportsCustomMounts()==true
// are responsible for honoring these mounts when provisioning a workspace.
//
// Scope and SubPath say where the source lives, so a backend that cannot
// bind the daemon host's filesystem (a cluster pod) can provide the same
// directory from its own storage instead of LocalPath.
type Mount struct {
	// LocalPath is the absolute host filesystem path to mount.
	LocalPath string
	// ContainerPath is the absolute path inside the container where the
	// host directory should appear.
	ContainerPath string
	// Scope classifies the source. The zero value, MountScopeHost, is a
	// plain host path.
	Scope MountScope
	// SubPath is the source's slash-separated path relative to its scope's
	// root, for scopes that have one ("" for MountScopeHost). For
	// MountScopeFleetShared the root is the fleet's mount root
	// (~/.fleet/workspaces/<fleet>), and LocalPath is that root joined with
	// SubPath.
	SubPath string
	// ReadOnly makes the mount read-only inside the container. A backend
	// that cannot mount read-only refuses the mount rather than mounting it
	// writable.
	ReadOnly bool
}
