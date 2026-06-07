package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// setupDebCache wires an instance into the fleet's shared deb (apt) cache: it
// ensures the apt-cacher-ng server is up, joins the instance to the fleet's
// shared docker network, and writes the apt http-proxy config inside the
// instance. Unlike buildkit there is no bind mount — the cache is reached over
// the network — so this runs AFTER Up (when the container id is known).
//
// It is a no-op when the feature does not apply (the backend cannot honor it or
// the fleet has the setting off). Errors are returned so the caller can surface
// a warning; per the feature contract this is best-effort and must NOT abort
// provisioning. ConfigureInstanceApt itself silently skips instances without a
// writable apt, so an error here is a genuine setup failure.
func setupDebCache(instanceBackend backend.Backend, fleetName, containerID, workspaceDir string) error {
	if !instanceBackend.SupportsCustomMounts() || !fleetDebCacheEnabled(fleetName) {
		return nil
	}
	if _, err := debcache.EnsureSharedServer(fleetName); err != nil {
		return err
	}
	if err := fleetnet.ConnectInstance(fleetName, containerID); err != nil {
		return err
	}
	return debcache.ConfigureInstanceApt(instanceBackend, workspaceDir, debcache.ProxyURL(fleetName))
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
