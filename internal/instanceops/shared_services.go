package instanceops

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

// sharedServicesFor returns the runtime a starting instance's shared services
// run on (nil for an instance that gets none). A package var so tests can
// substitute a recorder.
var sharedServicesFor = svcruntime.ForInstance

// reviveSharedServices brings back what a started instance needs of its fleet's
// shared services (settings says which are on). Every step is best-effort: a
// failure becomes a warning on the instance, never a failed start.
//
// On a runtime that reconfigures on start (the daemon host's docker), each
// enabled server is re-ensured, since the host may have rebooted or the
// container been removed while the instance was stopped, and the instance
// rejoins the network the caches are reached over. The two are independent:
// connecting is cheap and idempotent, so a failed ensure doesn't skip it. The
// in-instance apt and buildx config persist across stop/start, so nothing
// inside is rewritten for those.
//
// The image cache's mirror config is the exception, whatever the runtime: the
// create-time daemon.json write can be missed (a docker-in-docker daemon not up
// yet) or lost with the image, so the instance's dockerd is pointed at the
// mirror on every start. The merge is idempotent.
func reviveSharedServices(fleetName, instanceName string, instance *fleet.Instance, settings fleet.FleetSettings) {
	services := sharedServicesFor(instance)
	if services == nil {
		return
	}
	warn := func(format string, err error) {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf(format, err))
	}
	if services.ReconfigureOnStart() {
		if settings.BuildkitServer {
			if err := services.EnsureService(fleetName, fleet.ServiceBuildkit); err != nil {
				warn("buildkit server: %v", err)
			}
		}
		if settings.DebCacheServer {
			if err := services.EnsureService(fleetName, fleet.ServiceDebCache); err != nil {
				warn("deb cache: %v", err)
			}
			if err := services.ConnectInstance(fleetName, instance.ContainerID); err != nil {
				warn("deb cache network: %v", err)
			}
		}
		if settings.ImageCacheServer {
			if err := services.EnsureService(fleetName, fleet.ServiceImageCache); err != nil {
				warn("image cache: %v", err)
			}
			if err := services.ConnectInstance(fleetName, instance.ContainerID); err != nil {
				warn("image cache network: %v", err)
			}
		}
	}
	if settings.ImageCacheServer {
		configureImageMirror(services, fleetName, instanceName, instance)
	}
}

// configureImageMirror points instance's own dockerd at its fleet's image
// cache's endpoint on services, with the same sync-then-background poll as
// create (an instance without a dockerd never configures, which is fine; nor
// does one whose runtime points no instance at the mirror). A package var so
// tests can see the call without a container.
var configureImageMirror = func(services svcruntime.Runtime, fleetName, instanceName string, instance *fleet.Instance) {
	endpoint, err := services.Endpoint(fleetName, fleet.ServiceImageCache)
	if err != nil {
		flog.Info("image cache: mirror not configured on start", "fleet", fleetName, "instance", instanceName, "reason", err)
		return
	}
	imagecache.ConfigureInstanceDockerPolling(backendutil.NewForInstance(instance, false), instance.WorkspaceDir,
		endpoint.URL, endpoint.HostPort,
		func(configured bool, err error) {
			switch {
			case err != nil:
				state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
			case !configured:
				flog.Info("image cache: no in-instance dockerd appeared within poll window; mirror not configured",
					"fleet", fleetName, "instance", instanceName)
			default:
				flog.Info("instance image cache configured", "fleet", fleetName, "instance", instanceName)
			}
		})
}
