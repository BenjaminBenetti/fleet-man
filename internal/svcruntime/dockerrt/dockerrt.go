// Package dockerrt is the shared-service runtime for the daemon host's docker:
// each of a fleet's shared services is one container on the same docker that
// runs the fleet's devcontainers. It is a thin layer over the packages that
// have always done this work, and behaves exactly as they do: internal/buildkit
// (a privileged buildkitd whose socket directory is bind-mounted into each
// instance), internal/debcache and internal/imagecache (apt-cacher-ng and a
// registry:2 mirror, reached by container name), and internal/fleetnet (the
// per-fleet network those two caches and the instances share).
package dockerrt

import (
	"context"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
)

// Key is the docker runtime's svcruntime key.
const Key = "docker"

// ReconcileBudget is how long one TUI-connect sweep of the docker runtime may
// take: the bound the sweep has always had.
const ReconcileBudget = 3 * time.Minute

// service is how the docker runtime runs one shared service. mounts is nil for
// a service reached over the fleet network, endpoint for one reached through
// its mounts.
type service struct {
	ensure   func(fleetName string) (string, error)
	stop     func(fleetName string) error
	wipe     func(fleetName string) error
	mounts   func(fleetName string) []backend.Mount
	endpoint func(fleetName string) fleet.ServiceEndpoint
}

// services maps each shared service to the package that runs it. A package var
// so tests can see what each method calls without a docker daemon.
var services = map[fleet.SharedService]service{
	fleet.ServiceBuildkit: {
		ensure: buildkit.EnsureSharedServer,
		stop:   buildkit.StopSharedServer,
		wipe:   buildkit.DeleteCache,
		mounts: func(fleetName string) []backend.Mount {
			return []backend.Mount{buildkit.InstanceMount(fleetName)}
		},
	},
	fleet.ServiceDebCache: {
		ensure: debcache.EnsureSharedServer,
		stop:   debcache.StopSharedServer,
		wipe:   debcache.DeleteCache,
		endpoint: func(fleetName string) fleet.ServiceEndpoint {
			return fleet.ServiceEndpoint{URL: debcache.ProxyURL(fleetName)}
		},
	},
	fleet.ServiceImageCache: {
		ensure: imagecache.EnsureSharedServer,
		stop:   imagecache.StopSharedServer,
		wipe:   imagecache.DeleteCache,
		endpoint: func(fleetName string) fleet.ServiceEndpoint {
			return fleet.ServiceEndpoint{URL: imagecache.MirrorURL(fleetName), HostPort: imagecache.MirrorHostPort(fleetName)}
		},
	},
}

// connectInstance and removeNetwork are the fleet network seams, package vars
// for the same reason as services.
var (
	connectInstance = fleetnet.ConnectInstance
	removeNetwork   = fleetnet.RemoveNetwork
)

// Runtime runs shared services on the daemon host's docker. The zero value is
// ready to use; it holds no state (the packages it wraps serialize per fleet).
type Runtime struct{}

// Key returns Key.
func (Runtime) Key() string { return Key }

// PrepareFleet does nothing: the fleet network comes with the first service
// that needs it (EnsureService, ConnectInstance).
func (Runtime) PrepareFleet(string) error { return nil }

// EnsureService ensures svc's container for fleetName (the deb and image
// caches on the fleet network, which they create when missing).
func (Runtime) EnsureService(fleetName string, svc fleet.SharedService) error {
	s, err := lookup(svc)
	if err != nil {
		return err
	}
	_, err = s.ensure(fleetName)
	return err
}

// InstanceMounts returns the bind mount of the buildkit socket directory for
// ServiceBuildkit, and nil for the network-based caches (and an unknown
// service).
func (Runtime) InstanceMounts(fleetName string, svc fleet.SharedService) []backend.Mount {
	s, ok := services[svc]
	if !ok || s.mounts == nil {
		return nil
	}
	return s.mounts(fleetName)
}

// Endpoint is where an instance reaches svc: the deb and image caches by
// container name over the fleet network, buildkit through its socket mount
// (an empty Endpoint).
func (Runtime) Endpoint(fleetName string, svc fleet.SharedService) (fleet.ServiceEndpoint, error) {
	s, err := lookup(svc)
	if err != nil {
		return fleet.ServiceEndpoint{}, err
	}
	if s.endpoint == nil {
		return fleet.ServiceEndpoint{}, nil
	}
	return s.endpoint(fleetName), nil
}

// ConnectInstance joins the container to the fleet's network (creating it
// when missing). A blank containerID is a no-op.
func (Runtime) ConnectInstance(fleetName, containerID string) error {
	return connectInstance(fleetName, containerID)
}

// StopService force-removes svc's container for fleetName, keeping its cache
// directory under ~/.fleet/workspaces/<fleet>.
func (Runtime) StopService(fleetName string, svc fleet.SharedService) error {
	s, err := lookup(svc)
	if err != nil {
		return err
	}
	return s.stop(fleetName)
}

// WipeService clears svc's cache directory (keeping the directory itself,
// which running instances may bind) and re-ensures the container.
func (Runtime) WipeService(fleetName string, svc fleet.SharedService) error {
	s, err := lookup(svc)
	if err != nil {
		return err
	}
	return s.wipe(fleetName)
}

// ReleaseFleet does nothing: a fleet's containers and network on docker stay
// until the fleet goes (TeardownFleet), however many instances it has left.
func (Runtime) ReleaseFleet(string) error { return nil }

// TeardownFleet removes the fleet's network. Docker refuses while a container
// is still attached, so it runs after the instances and caches are gone.
func (Runtime) TeardownFleet(fleetName string) error {
	return removeNetwork(fleetName)
}

// ReconfigureOnStart is true: a cache container can be gone or stopped by the
// time an instance starts again (a host reboot, `docker rm`), so a starting
// instance re-ensures its fleet's services and rejoins the fleet network.
func (Runtime) ReconfigureOnStart() bool { return true }

// Reachable is always true. The daemon host's docker has never been probed
// before a sweep: when it is down, each EnsureService fails and says so.
func (Runtime) Reachable(context.Context) bool { return true }

// ReconcileBudget returns ReconcileBudget.
func (Runtime) ReconcileBudget() time.Duration { return ReconcileBudget }

func lookup(svc fleet.SharedService) (service, error) {
	s, ok := services[svc]
	if !ok {
		return service{}, fmt.Errorf("docker runtime: unknown shared service %q", svc)
	}
	return s, nil
}
