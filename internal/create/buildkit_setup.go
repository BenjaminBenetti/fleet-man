package create

import (
	"errors"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

// prepareBuildkitMounts ensures the fleet's shared buildkit server is up on
// services (the instance's shared-service runtime) and returns the mounts the
// instance reaches it through (on docker, the bind mount of its socket
// directory).
//
// It returns no mounts and no error when the feature does not apply — the
// instance has no shared-service runtime (cloud backends) or the fleet has the
// BuildkitServer setting off. A genuine failure to start the server is returned
// so the caller can surface a warning; per the feature contract this is
// best-effort and must NOT abort provisioning.
func prepareBuildkitMounts(services svcruntime.Runtime, fleetName string) ([]backend.Mount, error) {
	if services == nil || !fleetBuildkitEnabled(fleetName) {
		return nil, nil
	}
	if err := services.EnsureService(fleetName, fleet.ServiceBuildkit); err != nil {
		return nil, err
	}
	return services.InstanceMounts(fleetName, fleet.ServiceBuildkit), nil
}

// configureBuildkit points the instance's docker buildx at the fleet's shared
// buildkit server. It is a no-op (nil) when the feature does not apply. When it
// does, ConfigureInstanceBuildx itself silently skips images that lack
// docker/buildx, so an error here is only a real configuration failure — which
// the caller should treat as non-fatal.
func configureBuildkit(services svcruntime.Runtime, instanceBackend backend.Backend, fleetName, workspaceDir string) error {
	if services == nil || !fleetBuildkitEnabled(fleetName) {
		return nil
	}
	// A runtime that points no instance at its buildkit server leaves buildx
	// alone (prepareBuildkitMounts already warned why there is no server).
	if _, err := services.Endpoint(fleetName, fleet.ServiceBuildkit); err != nil {
		if errors.Is(err, svcruntime.ErrNoEndpoint) {
			return nil
		}
		return err
	}
	configured, err := buildkit.ConfigureInstanceBuildx(instanceBackend, workspaceDir)
	if err != nil {
		return err
	}
	if configured {
		flog.Info("instance buildx configured", "fleet", fleetName, "workspace", workspaceDir)
	}
	return nil
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
