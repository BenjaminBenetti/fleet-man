package server

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shared_services.go is the daemon's side of a fleet's shared services
// (buildkit, deb cache, image cache; see internal/svcruntime): the TUI-connect
// reconcile, the whole-fleet teardown, and the Delete cache RPCs.

// sharedServicesFor returns the runtime an instance's fleet's shared services
// run on for it (nil when it gets none), and hostSharedServices the daemon
// host's docker runtime, which a whole-fleet teardown and a cache wipe act on.
// Package vars so tests can substitute recorders.
var (
	sharedServicesFor  = svcruntime.ForInstance
	hostSharedServices = svcruntime.Docker
)

// sharedServiceText is how the daemon's messages name each shared service: its
// fleet setting, its cache, and its server.
var sharedServiceText = map[fleet.SharedService]struct{ setting, cache, server string }{
	fleet.ServiceBuildkit:   {setting: "buildkit server", cache: "buildkit cache", server: "buildkit server"},
	fleet.ServiceDebCache:   {setting: "deb cache", cache: "deb cache", server: "deb cache server"},
	fleet.ServiceImageCache: {setting: "image cache", cache: "image cache", server: "image cache server"},
}

// ensureConfiguredSharedServices re-ensures every fleet's enabled shared
// services wherever its instances use them. It is the fire-and-forget
// reconciliation kicked off when a TUI connects (see service.onTUIConnected),
// recovering from a server that was killed externally or lost to a host reboot
// while no instance create/clone/start would otherwise have revived it.
//
// Gating mirrors the per-instance paths: a fleet's services are ensured on a
// runtime only when at least one of its instances uses that runtime
// (svcruntime.ForInstance), so empty fleets and cloud-only fleets never spin up
// an unused server. Each runtime is swept in its own goroutine, so a slow one
// never holds up another, and one that isn't Reachable is skipped. Within a
// runtime the sweep is serial: fleet by fleet, each fleet's services in
// fleet.SharedServices order. Best-effort throughout: a state-load or ensure
// failure is logged, never surfaced (the caller does not wait on the result).
// EnsureService is idempotent and per-fleet serialized, so this is safe to run
// concurrently with instance lifecycle jobs and with itself.
//
// ctx bounds the sweep: it is checked between ensures, so the sweep stops
// promptly on server shutdown or the caller's timeout (it does not interrupt an
// ensure in progress).
func ensureConfiguredSharedServices(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("shared services reconcile: load state failed", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, sweep := range sharedServiceSweeps(st) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sweep.run(ctx)
		}()
	}
	wg.Wait()
}

// sharedServiceSweep is one runtime's share of a reconcile: the fleets that
// have an instance using it, each with the services it has on.
type sharedServiceSweep struct {
	runtime svcruntime.Runtime
	fleets  []fleetServices
}

// fleetServices is a fleet and the shared services it has on.
type fleetServices struct {
	name     string
	services []fleet.SharedService
}

// sharedServiceSweeps plans a reconcile of st: one sweep per runtime that any
// fleet with a service on has an instance using, in fleet-name order.
func sharedServiceSweeps(st *state.State) []*sharedServiceSweep {
	names := make([]string, 0, len(st.Fleets))
	for name := range st.Fleets {
		names = append(names, name)
	}
	sort.Strings(names)

	byKey := map[string]*sharedServiceSweep{}
	var sweeps []*sharedServiceSweep
	for _, name := range names {
		f := st.Fleets[name]
		if f == nil {
			continue
		}
		services := f.Settings.EnabledSharedServices()
		if len(services) == 0 {
			continue
		}
		planned := map[string]bool{}
		for _, inst := range f.Instances {
			if inst == nil {
				continue // tolerate a malformed/hand-edited state.json
			}
			rt := sharedServicesFor(inst)
			if rt == nil || planned[rt.Key()] {
				continue
			}
			planned[rt.Key()] = true
			sweep := byKey[rt.Key()]
			if sweep == nil {
				sweep = &sharedServiceSweep{runtime: rt}
				byKey[rt.Key()] = sweep
				sweeps = append(sweeps, sweep)
			}
			sweep.fleets = append(sweep.fleets, fleetServices{name: name, services: services})
		}
	}
	return sweeps
}

// run ensures each planned service on the sweep's runtime, stopping when ctx
// ends.
func (s *sharedServiceSweep) run(ctx context.Context) {
	key := s.runtime.Key()
	if !s.runtime.Reachable(ctx) {
		flog.Warn("shared services reconcile: runtime unreachable, skipped", "runtime", key)
		return
	}
	for _, f := range s.fleets {
		for _, svc := range f.services {
			if ctx.Err() != nil {
				return
			}
			if err := s.runtime.EnsureService(f.name, svc); err != nil {
				flog.Warn("shared services reconcile: ensure failed", "runtime", key, "fleet", f.name, "service", svc, "err", err)
			}
		}
	}
}

// teardownFleetSharedServices removes a fleet's shared service servers and
// then its network, on a whole-fleet destroy (the destroy job with
// destroy_fleet, and the DestroyFleet RPC that removes an empty fleet), once its
// instances are down. It acts on the daemon host's docker whatever the fleet's
// instances ran on, and it LEAVES each cache's directory on disk: a cache
// outlives the fleet and warms the next fleet of the same name, exactly like
// the persisted .claude/.codex/.config/gh mount dirs do.
//
// The buildkit server is removed only when the fleet has it on
// (buildkitEnabled, read before the record went away). The deb/image caches
// and the network are removed UNCONDITIONALLY: toggling a cache off does not
// stop its running container, so gating on the live setting would orphan a
// container the user disabled before destroying. Each step is idempotent (a
// missing container or network is not an error), so this is safe for fleets
// that never used a cache. The network goes LAST, after the cache containers,
// since docker refuses to remove a network with active endpoints. Best-effort:
// failures come back as warnings, never as an abort.
func teardownFleetSharedServices(fleetName string, buildkitEnabled bool) []string {
	services := hostSharedServices()
	var warnings []string
	for _, svc := range fleet.SharedServices() {
		if svc == fleet.ServiceBuildkit && !buildkitEnabled {
			continue
		}
		if err := services.StopService(fleetName, svc); err != nil {
			warnings = append(warnings, fmt.Sprintf("stop %s: %v", sharedServiceText[svc].server, err))
		}
	}
	if err := services.TeardownFleet(fleetName); err != nil {
		warnings = append(warnings, fmt.Sprintf("remove fleet network: %v", err))
	}
	return warnings
}

// DeleteBuildkitCache wipes a fleet's shared build cache and restarts the empty
// server. Synchronous: it returns only once the cache is gone and the server is
// back (or an error). The client uses a longer deadline for this call.
func (s *service) DeleteBuildkitCache(_ context.Context, req *fleetgrpc.DeleteBuildkitCacheRequest) (*fleetgrpc.DeleteBuildkitCacheReply, error) {
	if err := wipeSharedService(req.GetFleet(), fleet.ServiceBuildkit); err != nil {
		return nil, err
	}
	return &fleetgrpc.DeleteBuildkitCacheReply{}, nil
}

// DeleteDebCache wipes a fleet's shared deb (apt) cache and restarts the empty
// server. Synchronous; mirrors DeleteBuildkitCache.
func (s *service) DeleteDebCache(_ context.Context, req *fleetgrpc.DeleteDebCacheRequest) (*fleetgrpc.DeleteDebCacheReply, error) {
	if err := wipeSharedService(req.GetFleet(), fleet.ServiceDebCache); err != nil {
		return nil, err
	}
	return &fleetgrpc.DeleteDebCacheReply{}, nil
}

// DeleteImageCache wipes a fleet's shared docker image cache and restarts the
// empty server. Synchronous; mirrors DeleteBuildkitCache.
func (s *service) DeleteImageCache(_ context.Context, req *fleetgrpc.DeleteImageCacheRequest) (*fleetgrpc.DeleteImageCacheReply, error) {
	if err := wipeSharedService(req.GetFleet(), fleet.ServiceImageCache); err != nil {
		return nil, err
	}
	return &fleetgrpc.DeleteImageCacheReply{}, nil
}

// wipeSharedService is the Delete cache RPCs' shared body: it wipes fleetName's
// svc cache on the daemon host's docker and restarts the server empty. It only
// wipes for a fleet that has svc on: otherwise the restart step would spin up a
// server the fleet never asked for. (The TUI only shows the button when it is
// on; this protects direct/stale callers.)
func wipeSharedService(fleetName string, svc fleet.SharedService) error {
	if fleetName == "" {
		return status.Error(codes.InvalidArgument, "fleet is required")
	}
	st, err := state.Load()
	if err != nil {
		return status.Errorf(codes.Internal, "load state: %v", err)
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
	}
	if !f.Settings.SharedServiceEnabled(svc) {
		return status.Errorf(codes.FailedPrecondition, "fleet %q does not have the %s enabled", fleetName, sharedServiceText[svc].setting)
	}
	if err := hostSharedServices().WipeService(fleetName, svc); err != nil {
		return status.Errorf(codes.Internal, "delete %s: %v", sharedServiceText[svc].cache, err)
	}
	return nil
}
