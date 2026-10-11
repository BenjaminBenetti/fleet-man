package fleet

import "errors"

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

// CacheUsage is how full one of a fleet's shared service volumes is, in one
// place the fleet's services run on volumes of a fixed size (a cluster).
type CacheUsage struct {
	Service SharedService
	// Cluster is the connection the volume is on, Location its display name.
	Cluster  string
	Location string
	// UsedBytes and CapacityBytes are the volume's use and size (0 when
	// unknown).
	UsedBytes     int64
	CapacityBytes int64
	// Expandable: the volume grows by half when it is nearly full.
	Expandable bool
	// State is the service's server: "ready", "starting", or why it is
	// neither.
	State string
	// Warning is the line the fleet's Caching row shows for the volume ("93%
	// full; its StorageClass cannot grow it"); empty when all is well.
	Warning string
}

// CacheFullPercent is how full a cache volume gets before it grows (where its
// StorageClass allows) or warns.
const CacheFullPercent = 90

// Percent is how full u's volume is (0 when its size is unknown).
func (u CacheUsage) Percent() int {
	if u.CapacityBytes <= 0 {
		return 0
	}
	return int(u.UsedBytes * 100 / u.CapacityBytes)
}

// ServiceEndpoint is where an instance reaches one of its fleet's shared
// services, for the configuration inside the instance that points it there
// (svcruntime.Endpoint).
type ServiceEndpoint struct {
	// URL is the service's base URL as the instance reaches it (the deb
	// cache's HTTP proxy, the image cache's mirror); empty for a service the
	// instance reaches through a mount (buildkit's socket on docker).
	URL string
	// HostPort is URL's host:port, for configuration that names it bare (an
	// insecure registry mirror).
	HostPort string
	// Fallback says the service may not answer while the instance is up (a
	// cluster's server comes up on the cluster's time, after the instance is
	// pointed at it, and no start revives it): the instance checks it answers
	// before each use and goes direct when it does not (apt's
	// Proxy-Auto-Detect through `fleet net-probe`; dockerd falls back from
	// its mirrors by itself). Docker's servers are revived on every start,
	// so the instance uses them as they are.
	Fallback bool
}

// ErrNoServiceEndpoint is a shared service a runtime runs but does not point
// instances at (svcruntime.ErrNoEndpoint).
var ErrNoServiceEndpoint = errors.New("instances are not pointed at this shared service here")
