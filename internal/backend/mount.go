package backend

// Mount describes a host directory bind-mounted into a container at a
// specified target path. Backends that report SupportsCustomMounts()==true
// are responsible for honoring these mounts when provisioning a workspace.
type Mount struct {
	// LocalPath is the absolute host filesystem path to mount.
	LocalPath string
	// ContainerPath is the absolute path inside the container where the
	// host directory should appear.
	ContainerPath string
}
