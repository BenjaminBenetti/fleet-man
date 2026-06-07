package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// setupImageCache wires an instance into the fleet's shared docker image cache:
// it ensures the registry pull-through server is up, joins the instance to the
// fleet's shared docker network, and points the instance's own dockerd at the
// mirror. Runs AFTER Up (the container id is needed to join the network).
//
// It is a no-op when the feature does not apply. Errors are returned so the
// caller can surface a warning; best-effort and must NOT abort provisioning.
// ConfigureInstanceDocker silently skips instances without their own dockerd
// (docker-outside-of-docker / no docker), so an error here is a genuine failure.
func setupImageCache(instanceBackend backend.Backend, fleetName, containerID, workspaceDir string) error {
	if !instanceBackend.SupportsCustomMounts() || !fleetImageCacheEnabled(fleetName) {
		return nil
	}
	if _, err := imagecache.EnsureSharedServer(fleetName); err != nil {
		return err
	}
	if err := fleetnet.ConnectInstance(fleetName, containerID); err != nil {
		return err
	}
	return imagecache.ConfigureInstanceDocker(instanceBackend, workspaceDir,
		imagecache.MirrorURL(fleetName), imagecache.MirrorHostPort(fleetName))
}

// fleetImageCacheEnabled reports whether the named fleet has the ImageCacheServer
// setting enabled. A state-load miss is treated as "disabled".
func fleetImageCacheEnabled(fleetName string) bool {
	st, err := state.Load()
	if err != nil {
		return false
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return false
	}
	return f.Settings.ImageCacheServer
}
