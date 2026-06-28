package create

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
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
func setupImageCache(instanceBackend backend.Backend, fleetName, instanceName, containerID, workspaceDir string) error {
	if !instanceBackend.SupportsCustomMounts() || !fleetImageCacheEnabled(fleetName) {
		return nil
	}
	if _, err := imagecache.EnsureSharedServer(fleetName); err != nil {
		return err
	}
	if err := fleetnet.ConnectInstance(fleetName, containerID); err != nil {
		return err
	}
	// Point the instance's own dockerd at the mirror. A docker-in-docker daemon
	// may not have finished starting yet, so poll rather than probe once: the
	// first few probes run synchronously (so a fast dind is wired up before the
	// user starts working), then a background rescue loop keeps trying for the
	// rest of the window. Best-effort — an instance with no dockerd never
	// configures, which is fine.
	imagecache.ConfigureInstanceDockerPolling(instanceBackend, workspaceDir,
		imagecache.MirrorURL(fleetName), imagecache.MirrorHostPort(fleetName),
		imageCacheConfigureResult(fleetName, instanceName))
	return nil
}

// imageCacheConfigureResult builds the ConfigureInstanceDockerPolling callback:
// a configure failure becomes a TUI-visible warning, while never finding a
// dockerd within the window is logged at info (it is the expected outcome for an
// instance without its own dockerd, so it must not surface as a warning).
func imageCacheConfigureResult(fleetName, instanceName string) func(bool, error) {
	return func(configured bool, err error) {
		switch {
		case err != nil:
			state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
		case !configured:
			flog.Info("image cache: no in-instance dockerd appeared within poll window; mirror not configured",
				"fleet", fleetName, "instance", instanceName)
		default:
			flog.Info("instance image cache configured", "fleet", fleetName, "instance", instanceName)
		}
	}
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
