// Package svcruntime runs a fleet's shared services (fleet.SharedService: the
// buildkit server, the deb cache, the image cache) where the fleet's instances
// can reach them. A Runtime is one such place. Today there is one, the daemon
// host's docker (internal/svcruntime/dockerrt), which runs each service as a
// container next to the devcontainers that use it.
//
// Callers never ask what kind of backend an instance runs on to decide whether
// it gets shared services: they ask ForInstance for the instance's runtime, and
// an instance with none (a cloud workspace that can't reach the daemon host's
// docker) simply goes without. A whole-fleet teardown or a cache wipe, which
// has no single instance to ask about, acts on Docker directly.
//
// Everything here is daemon-side orchestration; clients only flip the
// FleetSettings toggles through fleetgrpc.
package svcruntime

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// Runtime runs fleets' shared services in one place. Every method is
// idempotent and safe to call concurrently; a runtime serializes the work on
// one fleet itself. An unknown fleet.SharedService is an error.
type Runtime interface {
	// Key names the place the services run. Two instances whose runtimes have
	// the same Key share one set of their fleet's services.
	Key() string

	// EnsureService makes fleetName's svc exist and run, reusing a running
	// server, starting a stopped one and creating a missing one.
	EnsureService(fleetName string, svc fleet.SharedService) error

	// InstanceMounts returns the mounts an instance needs to reach
	// fleetName's svc (a socket directory, say), to append to the mounts its
	// backend provisions it with. Nil when the instance reaches svc over the
	// network instead.
	InstanceMounts(fleetName string, svc fleet.SharedService) []backend.Mount

	// ConnectInstance lets the instance with containerID reach fleetName's
	// network-based services (the deb and image caches).
	ConnectInstance(fleetName, containerID string) error

	// StopService removes fleetName's svc server, keeping its cache data so a
	// later fleet of the same name starts warm. A server that is not there is
	// not an error.
	StopService(fleetName string, svc fleet.SharedService) error

	// WipeService deletes fleetName's svc cache data and brings the server
	// back empty. It returns once the cache is gone and the server is back.
	WipeService(fleetName string, svc fleet.SharedService) error

	// TeardownFleet removes what ConnectInstance set up for fleetName (its
	// shared network). It runs on a whole-fleet destroy, after the fleet's
	// instances and service servers are gone.
	TeardownFleet(fleetName string) error

	// ReconfigureOnStart reports whether an instance starting on this runtime
	// has to re-ensure its fleet's services and ConnectInstance again, because
	// they may not have outlived the instance being stopped (a host reboot,
	// a docker restart, a container removed by hand).
	ReconfigureOnStart() bool

	// Reachable reports whether the runtime can be worked on now. A
	// reconcile sweep skips one that can't, instead of waiting out each
	// service's failure.
	Reachable(ctx context.Context) bool
}
