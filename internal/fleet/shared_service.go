package fleet

// SharedService names one of a fleet's shared services: a per-fleet server
// that every instance of the fleet uses, switched on by its own FleetSettings
// toggle. Where a service runs is up to the runtime that hosts it (see
// internal/svcruntime); the fleet only says which ones it wants.
type SharedService string

const (
	// ServiceBuildkit is the shared buildkit server docker buildx builds on
	// (FleetSettings.BuildkitServer).
	ServiceBuildkit SharedService = "buildkit"
	// ServiceDebCache is the shared apt-cacher-ng apt goes through
	// (FleetSettings.DebCacheServer).
	ServiceDebCache SharedService = "debcache"
	// ServiceImageCache is the shared registry pull-through mirror of Docker
	// Hub (FleetSettings.ImageCacheServer).
	ServiceImageCache SharedService = "imagecache"
)

// SharedServices lists every shared service in the order fleet brings them up.
func SharedServices() []SharedService {
	return []SharedService{ServiceBuildkit, ServiceDebCache, ServiceImageCache}
}

// SharedServiceEnabled reports whether s switches svc on. An unknown service is
// off.
func (s FleetSettings) SharedServiceEnabled(svc SharedService) bool {
	switch svc {
	case ServiceBuildkit:
		return s.BuildkitServer
	case ServiceDebCache:
		return s.DebCacheServer
	case ServiceImageCache:
		return s.ImageCacheServer
	default:
		return false
	}
}

// EnabledSharedServices returns the services s switches on, in SharedServices
// order (nil when none is on).
func (s FleetSettings) EnabledSharedServices() []SharedService {
	var on []SharedService
	for _, svc := range SharedServices() {
		if s.SharedServiceEnabled(svc) {
			on = append(on, svc)
		}
	}
	return on
}
