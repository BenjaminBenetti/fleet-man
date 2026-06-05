package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// prepareBuildkitMount ensures the fleet's shared buildkit server is up and
// returns the bind mount that exposes its socket directory inside the instance.
//
// It returns ok=false (with no error) when the feature does not apply — the
// backend cannot honor custom mounts (cloud backends) or the fleet has the
// BuildkitServer setting off. A genuine failure to start the server is returned
// so the caller can surface a warning; per the feature contract this is
// best-effort and must NOT abort provisioning.
func prepareBuildkitMount(instanceBackend backend.Backend, fleetName string) (backend.Mount, bool, error) {
	if !instanceBackend.SupportsCustomMounts() || !fleetBuildkitEnabled(fleetName) {
		return backend.Mount{}, false, nil
	}
	if _, err := buildkit.EnsureSharedServer(fleetName); err != nil {
		return backend.Mount{}, false, err
	}
	return buildkit.InstanceMount(fleetName), true, nil
}

// configureBuildkit points the instance's docker buildx at the fleet's shared
// buildkit server. It is a no-op (nil) when the feature does not apply. When it
// does, ConfigureInstanceBuildx itself silently skips images that lack
// docker/buildx, so an error here is only a real configuration failure — which
// the caller should treat as non-fatal.
func configureBuildkit(instanceBackend backend.Backend, fleetName, workspaceDir string) error {
	if !instanceBackend.SupportsCustomMounts() || !fleetBuildkitEnabled(fleetName) {
		return nil
	}
	return buildkit.ConfigureInstanceBuildx(instanceBackend, workspaceDir)
}

// fleetBuildkitEnabled reports whether the named fleet has the BuildkitServer
// setting enabled. A state-load miss is treated as "disabled" so a transient
// read failure never spins up a buildkit container the user didn't ask for.
func fleetBuildkitEnabled(fleetName string) bool {
	st, err := state.Load()
	if err != nil {
		return false
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return false
	}
	return f.Settings.BuildkitServer
}
