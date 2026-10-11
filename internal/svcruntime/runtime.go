// Package svcruntime runs a fleet's shared services (fleet.SharedService: the
// buildkit server, the deb cache, the image cache) where the fleet's instances
// can reach them. A Runtime is one such place: the daemon host's docker
// (internal/svcruntime/dockerrt), which runs each service as a container next
// to the devcontainers that use it, or one Kubernetes connection
// (internal/svcruntime/kubert), which runs each as a Deployment in the
// connection's namespace beside the fleet's pods, together with what the
// fleet's pods need there whatever services are on (sibling DNS, the
// NetworkPolicies). A fleet with instances in several places has one set of
// services in each.
//
// Callers never ask what kind of backend an instance runs on to decide whether
// it gets shared services: they ask ForInstance for the instance's runtime, and
// an instance with none (a cloud workspace that can't reach the daemon host's
// docker) simply goes without. A whole-fleet teardown or a cache wipe, which
// has no single instance to ask about, acts on every runtime the fleet's
// instances use or used (ForInstance, PresentClusters), and on Docker.
//
// Everything here is daemon-side orchestration; clients only flip the
// FleetSettings toggles through fleetgrpc.
package svcruntime

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// Endpoint is where an instance reaches one of its fleet's shared services
// (fleet.ServiceEndpoint, which the runtimes build without importing this
// package).
type Endpoint = fleet.ServiceEndpoint

// ErrNoEndpoint is a service a runtime runs but does not point instances at:
// their configuration is left alone, and they go without it.
var ErrNoEndpoint = fleet.ErrNoServiceEndpoint

// Runtime runs fleets' shared services in one place. Every method is
// idempotent and safe to call concurrently; a runtime serializes the work on
// one fleet itself. An unknown fleet.SharedService is an error.
type Runtime interface {
	// Key names the place the services run. Two instances whose runtimes have
	// the same Key share one set of their fleet's services.
	Key() string

	// PrepareFleet makes what fleetName's instances on this runtime need
	// before any of them is created there, whichever services the fleet has
	// on (on a cluster: the fleet's anchor, its headless Service for sibling
	// DNS and its NetworkPolicies). A create fails when it does. Nothing on
	// docker, whose fleet network comes with the first service.
	PrepareFleet(fleetName string) error

	// EnsureService makes fleetName's svc exist and run, reusing a running
	// server, starting a stopped one and creating a missing one.
	EnsureService(fleetName string, svc fleet.SharedService) error

	// InstanceMounts returns the mounts an instance needs to reach
	// fleetName's svc (a socket directory, say), to append to the mounts its
	// backend provisions it with. Nil when the instance reaches svc over the
	// network instead.
	InstanceMounts(fleetName string, svc fleet.SharedService) []backend.Mount

	// Endpoint is where an instance on this runtime reaches fleetName's svc
	// (ErrNoEndpoint: nowhere it is pointed at). It does no I/O.
	Endpoint(fleetName string, svc fleet.SharedService) (Endpoint, error)

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

	// ReleaseFleet runs when the last of fleetName's instances on this
	// runtime is destroyed and the fleet lives on: what the runtime keeps for
	// the fleet only while it has instances there goes (on a cluster, every
	// object of the fleet but its cache volumes, which warm the next
	// instance). Nothing on docker, where the fleet's services stay until
	// the fleet goes.
	ReleaseFleet(fleetName string) error

	// TeardownFleet removes what ConnectInstance and PrepareFleet set up for
	// fleetName (its shared network; on a cluster every object of the fleet,
	// its cache volumes only where the connection deletes them with the
	// fleet). It runs on a whole-fleet destroy, after the fleet's instances
	// and service servers are gone.
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

	// ReconcileBudget bounds one reconcile sweep of this runtime.
	ReconcileBudget() time.Duration
}

// UsageReporter is a runtime whose services run on volumes of a fixed size
// (a cluster's): it says how full each of a fleet's is.
type UsageReporter interface {
	CacheUsage(ctx context.Context, fleetName string) ([]fleet.CacheUsage, error)
}

// Maintainer is a runtime whose services need upkeep the daemon schedules
// (a cluster's: growing a nearly full cache volume, the deb cache's daily
// expiry run).
type Maintainer interface {
	Maintain(ctx context.Context, fleetName string, services []fleet.SharedService) error
}
