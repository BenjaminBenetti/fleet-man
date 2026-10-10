package fleetlaunch

// Stager is an optional capability for backends that stage the fleet binary
// into an instance their own way, instead of EnsureFresh copying the daemon's
// own executable to RemotePath. Kubernetes pods get a static fleet-pod build of
// the daemon's version, kept content-addressed under /opt/fleet/bin with
// RemotePath linked to it, because the daemon's own executable (on Linux, the
// full build that links client-go) is not what a pod should run.
//
// EnsureFresh hands every call to a Stager, so the places that stage today
// (provisioning, clone, start, browser open, mic and audio output) stage
// through it unchanged.
type Stager interface {
	// StageFleet does what EnsureFresh does for the backend's instance at
	// workspaceDir: makes RemotePath run a fleet binary of the daemon's
	// version, refreshing a missing or older one, and reports whether it
	// staged one. beforeRefresh has EnsureFresh's meaning: when non-nil, it
	// runs just before a refresh replaces the binary, and not at all when the
	// binary is already current.
	StageFleet(workspaceDir string, beforeRefresh func() error) (staged bool, err error)
}
