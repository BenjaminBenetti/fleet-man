package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

// setupDebCache wires an instance into the fleet's shared deb (apt) cache: it
// ensures the apt-cacher-ng server is up on services (the instance's
// shared-service runtime), connects the instance to it (on docker, joins the
// fleet's shared network), and writes the apt http-proxy config inside the
// instance, pointing at the cache's endpoint on the runtime (a runtime that
// points no instance at it says so, and the instance's apt goes direct). Unlike buildkit there is no mount — the cache is reached over the
// network — so this runs AFTER Up (when the container id is known).
//
// It is a no-op when the feature does not apply (the instance has no
// shared-service runtime or the fleet has the setting off). Errors are returned
// so the caller can surface a warning; per the feature contract this is
// best-effort and must NOT abort provisioning. ConfigureInstanceApt itself
// silently skips instances without a writable apt, so an error here is a
// genuine setup failure.
func setupDebCache(services svcruntime.Runtime, instanceBackend backend.Backend, fleetName, containerID, workspaceDir string) error {
	if services == nil || !fleetDebCacheEnabled(fleetName) {
		return nil
	}
	if err := services.EnsureService(fleetName, fleet.ServiceDebCache); err != nil {
		return err
	}
	if err := services.ConnectInstance(fleetName, containerID); err != nil {
		return err
	}
	endpoint, err := services.Endpoint(fleetName, fleet.ServiceDebCache)
	if err != nil {
		return err
	}
	configured, err := debcache.ConfigureInstanceApt(instanceBackend, workspaceDir, endpoint.URL)
	if err != nil {
		return err
	}
	if configured {
		flog.Info("instance apt cache configured", "fleet", fleetName, "workspace", workspaceDir)
	}
	return nil
}

// fleetDebCacheEnabled reports whether the named fleet has the DebCacheServer
// setting enabled. A state-load miss is treated as "disabled" so a transient
// read failure never spins up a cache container the user didn't ask for.
func fleetDebCacheEnabled(fleetName string) bool {
	st, err := state.Load()
	if err != nil {
		return false
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return false
	}
	return f.Settings.DebCacheServer
}
