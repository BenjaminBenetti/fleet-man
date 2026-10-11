package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shared_services.go is the daemon's side of a fleet's shared services
// (buildkit, deb cache, image cache; see internal/svcruntime): the TUI-connect
// reconcile, the release of a runtime a fleet's last instance there left, the
// whole-fleet teardown, the Delete cache RPCs, the cache usage report, and the
// upkeep of the runtimes that need it (a cluster's caches).

// sharedServicesFor returns the runtime an instance's fleet's shared services
// run on for it (nil when it gets none), hostSharedServices the daemon host's
// docker runtime, which a whole-fleet teardown and a cache wipe always act on,
// clusterSharedServices a Kubernetes connection's runtime, and
// fleetPresentClusters the connections a fleet's objects were recorded on.
// Package vars so tests can substitute recorders.
var (
	sharedServicesFor     = svcruntime.ForInstance
	hostSharedServices    = svcruntime.Docker
	clusterSharedServices = svcruntime.ForCluster
	fleetPresentClusters  = svcruntime.PresentClusters
)

// fleetRuntimes are the runtimes a whole-fleet teardown acts on: the daemon
// host's docker always (as it always has, whatever the fleet's instances ran
// on), each runtime instances use, and each connection the fleet's objects
// were recorded on (its cache volumes may be there after its instances left).
func fleetRuntimes(fleetName string, instances []*fleet.Instance) []svcruntime.Runtime {
	host := hostSharedServices()
	out := []svcruntime.Runtime{host}
	seen := map[string]bool{host.Key(): true}
	add := func(rt svcruntime.Runtime) {
		if rt != nil && !seen[rt.Key()] {
			seen[rt.Key()] = true
			out = append(out, rt)
		}
	}
	for _, inst := range instances {
		if inst != nil {
			add(sharedServicesFor(inst))
		}
	}
	for _, id := range fleetPresentClusters(fleetName) {
		add(clusterSharedServices(id))
	}
	return out
}

// instanceRuntimes are the distinct runtimes instances use, or the daemon
// host's docker when none uses one (what a cache wipe has always acted on for
// a fleet without such instances).
func instanceRuntimes(instances []*fleet.Instance) []svcruntime.Runtime {
	var out []svcruntime.Runtime
	seen := map[string]bool{}
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		rt := sharedServicesFor(inst)
		if rt == nil || seen[rt.Key()] {
			continue
		}
		seen[rt.Key()] = true
		out = append(out, rt)
	}
	if len(out) == 0 {
		out = []svcruntime.Runtime{hostSharedServices()}
	}
	return out
}

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
// fleet has an instance using, in fleet-name order, each fleet with the
// services it has on (none: the sweep only prepares the fleet there).
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

// run prepares each planned fleet on the sweep's runtime (which re-applies
// what a cluster keeps for it: sibling DNS, its NetworkPolicies) and ensures
// its services, within the runtime's ReconcileBudget, stopping when ctx ends.
func (s *sharedServiceSweep) run(ctx context.Context) {
	key := s.runtime.Key()
	ctx, cancel := context.WithTimeout(ctx, s.runtime.ReconcileBudget())
	defer cancel()
	if !s.runtime.Reachable(ctx) {
		flog.Warn("shared services reconcile: runtime unreachable, skipped", "runtime", key)
		return
	}
	for _, f := range s.fleets {
		if ctx.Err() != nil {
			return
		}
		if err := s.runtime.PrepareFleet(f.name); err != nil {
			flog.Warn("shared services reconcile: prepare failed", "runtime", key, "fleet", f.name, "err", err)
		}
		for _, svc := range f.services {
			if ctx.Err() != nil {
				return
			}
			if err := s.runtime.EnsureService(f.name, svc); errors.Is(err, svcruntime.ErrNoEndpoint) {
				// The runtime does not run it for the fleet (a cluster
				// that does not allow shared Buildkit): no failure.
				flog.Info("shared services reconcile: not run here", "runtime", key, "fleet", f.name, "service", svc, "reason", err)
			} else if err != nil {
				flog.Warn("shared services reconcile: ensure failed", "runtime", key, "fleet", f.name, "service", svc, "err", err)
			}
		}
	}
}

// teardownFleetSharedServices removes a fleet's shared service servers and
// then its network, on a whole-fleet destroy (the destroy job with
// destroy_fleet, and the DestroyFleet RPC that removes an empty fleet), once its
// instances are down, on every runtime the fleet has (fleetRuntimes; instances
// are the fleet's, read before its record went). On the daemon host's docker it
// LEAVES each cache's directory on disk: a cache outlives the fleet and warms
// the next fleet of the same name, exactly like the persisted
// .claude/.codex/.config/gh mount dirs do. A cluster keeps its cache volumes
// likewise, unless the connection deletes them with the fleet.
//
// The buildkit server is removed only when the fleet has it on
// (buildkitEnabled, read before the record went away). The deb/image caches
// and the network are removed UNCONDITIONALLY: toggling a cache off does not
// stop its running container, so gating on the live setting would orphan a
// container the user disabled before destroying. Each step is idempotent (a
// missing container or network is not an error), so this is safe for fleets
// that never used a cache. The network goes LAST, after the cache containers,
// since docker refuses to remove a network with active endpoints. A runtime
// that is not reachable now goes straight to its TeardownFleet (a cluster's
// records what it owes and deletes it once it can). Best-effort: failures come
// back as warnings, never as an abort.
func teardownFleetSharedServices(fleetName string, buildkitEnabled bool, instances []*fleet.Instance) []string {
	var warnings []string
	for _, services := range fleetRuntimes(fleetName, instances) {
		prefix := ""
		if services.Key() != hostSharedServices().Key() {
			prefix = services.Key() + ": "
		}
		if services.Reachable(context.Background()) {
			for _, svc := range fleet.SharedServices() {
				if svc == fleet.ServiceBuildkit && !buildkitEnabled {
					continue
				}
				if err := services.StopService(fleetName, svc); err != nil {
					warnings = append(warnings, fmt.Sprintf("%sstop %s: %v", prefix, sharedServiceText[svc].server, err))
				}
			}
		}
		if err := services.TeardownFleet(fleetName); err != nil {
			if prefix == "" {
				warnings = append(warnings, fmt.Sprintf("remove fleet network: %v", err))
			} else {
				warnings = append(warnings, prefix+err.Error())
			}
		}
	}
	return warnings
}

// prepareClusterFleets re-prepares every fleet with an instance on
// Kubernetes connection clusterID (svcruntime.Runtime.PrepareFleet), so a
// change of what the fleets' pods get there (its metadata block) reaches the
// pods already running. Best-effort, logged.
func prepareClusterFleets(clusterID string) {
	rt := clusterSharedServices(clusterID)
	if rt == nil {
		return
	}
	for _, name := range clusterFleets(clusterID, nil) {
		if err := rt.PrepareFleet(name); err != nil {
			flog.Warn("shared services: re-preparing a fleet on a connection failed", "runtime", rt.Key(), "fleet", name, "err", err)
			continue
		}
		flog.Info("shared services: re-prepared a fleet on a connection", "runtime", rt.Key(), "fleet", name)
	}
}

// reensureClusterBuildkit re-ensures the shared Buildkit of every fleet with
// it on and an instance on Kubernetes connection clusterID, so a change of
// whether the connection runs it (its Allow shared Buildkit) reaches the
// server now: turned off, the server goes; turned on, it comes back, and the
// fleet's running instances there get their credentials at the next check
// (renewSharedServiceCredentials, run right after). Best-effort, logged.
func reensureClusterBuildkit(ctx context.Context, clusterID string) {
	rt := clusterSharedServices(clusterID)
	if rt == nil {
		return
	}
	for _, name := range clusterFleets(clusterID, func(f *fleet.Fleet) bool { return f.Settings.BuildkitServer }) {
		switch err := rt.EnsureService(name, fleet.ServiceBuildkit); {
		case errors.Is(err, svcruntime.ErrNoEndpoint):
			flog.Info("shared services: the connection no longer runs the fleet's Buildkit", "runtime", rt.Key(), "fleet", name, "reason", err)
		case err != nil:
			flog.Warn("shared services: re-ensuring a fleet's Buildkit on a connection failed", "runtime", rt.Key(), "fleet", name, "err", err)
		default:
			flog.Info("shared services: re-ensured a fleet's Buildkit on a connection", "runtime", rt.Key(), "fleet", name)
		}
	}
	renewSharedServiceCredentials(ctx)
}

// clusterFleets are the fleets, by name, with an instance on Kubernetes
// connection clusterID (and that want, when given).
func clusterFleets(clusterID string, want func(*fleet.Fleet) bool) []string {
	st, err := state.Load()
	if err != nil {
		flog.Warn("shared services: load state failed", "err", err)
		return nil
	}
	var names []string
	for name, f := range st.Fleets {
		if f == nil || (want != nil && !want(f)) {
			continue
		}
		for _, inst := range f.Instances {
			if inst != nil && inst.Backend == fleet.BackendKubernetes && inst.KubeCluster == clusterID {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// releaseFleetRuntimes runs after a single-instance destroy removed the
// records of destroyed: each runtime one of them used is released
// (svcruntime.Runtime.ReleaseFleet), which a runtime that keeps something for
// a fleet only while it has instances there acts on once the last one is gone
// (a cluster: the fleet's objects but its cache volumes). Warnings say what
// is put off.
func releaseFleetRuntimes(fleetName string, destroyed []*fleet.Instance) []string {
	var warnings []string
	seen := map[string]bool{}
	for _, inst := range destroyed {
		if inst == nil {
			continue
		}
		rt := sharedServicesFor(inst)
		if rt == nil || seen[rt.Key()] {
			continue
		}
		seen[rt.Key()] = true
		if err := rt.ReleaseFleet(fleetName); err != nil {
			warnings = append(warnings, rt.Key()+": "+err.Error())
		}
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
// svc cache, and restarts the server empty, on every runtime the fleet's
// instances reach a copy of it on (the daemon host's docker for a fleet
// without such instances, as always). It only wipes for a fleet that has svc
// on: otherwise the restart step would spin up a server the fleet never asked
// for. (The TUI only shows the button when it is on; this protects
// direct/stale callers.) The runtimes are wiped concurrently; a runtime that
// cannot be reached now is reported, and the others are still wiped.
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
	runtimes := instanceRuntimes(f.Instances)
	// The runtimes wipe at once: a cluster's waits for its new server (up
	// to 270s), and the clients give the whole call 300s.
	outcomes := make([]string, len(runtimes))
	var wg sync.WaitGroup
	for i, rt := range runtimes {
		prefix := ""
		if len(runtimes) > 1 || rt.Key() != hostSharedServices().Key() {
			prefix = rt.Key() + ": "
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !rt.Reachable(context.Background()) {
				outcomes[i] = prefix + "not reachable now"
				return
			}
			if err := rt.WipeService(fleetName, svc); err != nil {
				outcomes[i] = prefix + err.Error()
			}
		}()
	}
	wg.Wait()
	var failed []string
	for _, o := range outcomes {
		if o != "" {
			failed = append(failed, o)
		}
	}
	if len(failed) > 0 {
		return status.Errorf(codes.Internal, "delete %s: %s", sharedServiceText[svc].cache, strings.Join(failed, "; "))
	}
	return nil
}

// GetCacheUsage reports how full each of a fleet's cache volumes is, on each
// runtime of its instances whose services run on volumes of a fixed size (a
// cluster's). A runtime that cannot say is left out, with its reason.
func (s *service) GetCacheUsage(ctx context.Context, req *fleetgrpc.GetCacheUsageRequest) (*fleetgrpc.GetCacheUsageReply, error) {
	fleetName := req.GetFleet()
	if fleetName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet is required")
	}
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
	}
	ctx, cancel := context.WithTimeout(ctx, cacheUsageTimeout)
	defer cancel()
	reply := &fleetgrpc.GetCacheUsageReply{}
	for _, rt := range instanceRuntimes(f.Instances) {
		reporter, ok := rt.(svcruntime.UsageReporter)
		if !ok {
			continue
		}
		if !rt.Reachable(ctx) {
			reply.Unavailable = append(reply.Unavailable, rt.Key()+": not reachable now")
			continue
		}
		usage, err := reporter.CacheUsage(ctx, fleetName)
		if err != nil {
			reply.Unavailable = append(reply.Unavailable, rt.Key()+": "+err.Error())
		}
		for _, u := range usage {
			reply.Caches = append(reply.Caches, protoconv.CacheUsageToProto(u))
		}
	}
	return reply, nil
}

// cacheUsageTimeout bounds a cache usage report (a du in each server's pod).
const cacheUsageTimeout = 30 * time.Second

// sharedServiceUpkeepInterval paces the upkeep of the runtimes that need it
// (FLEET_KUBE_CACHE_CHECK_INTERVAL overrides it).
var sharedServiceUpkeepInterval = 30 * time.Minute

// sharedServiceUpkeepEvery is the upkeep interval, the environment's when it
// sets a valid one.
func sharedServiceUpkeepEvery() time.Duration {
	if v := os.Getenv("FLEET_KUBE_CACHE_CHECK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		flog.Warn("shared services upkeep: ignoring FLEET_KUBE_CACHE_CHECK_INTERVAL", "value", v)
	}
	return sharedServiceUpkeepInterval
}

// credentialCheckInterval paces the check of the instances' shared Buildkit
// credentials (renewSharedServiceCredentials, which renews only those due),
// so a daemon back from a long outage renews them minutes after its
// clusters are usable again.
var credentialCheckInterval = 5 * time.Minute

// runSharedServiceUpkeep runs maintainSharedServices (and the clusters'
// fleet shares' upkeep, maintainShares) every sharedServiceUpkeepEvery, and
// renewSharedServiceCredentials at once and then at least every
// credentialCheckInterval, until ctx ends.
func runSharedServiceUpkeep(ctx context.Context) {
	every := sharedServiceUpkeepEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	c := time.NewTicker(min(every, credentialCheckInterval))
	defer c.Stop()
	renewSharedServiceCredentials(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.C:
			renewSharedServiceCredentials(ctx)
		case <-t.C:
			maintainSharedServices(ctx)
			maintainShares(ctx)
		}
	}
}

// maintainSharedServices runs the upkeep (svcruntime.Maintainer) of every
// runtime that needs it, for each fleet with services on and an instance
// there: a cluster's caches grow when nearly full, or warn, and its deb cache
// runs its daily expiry. Runtimes run concurrently, each fleet in turn, each
// within its ReconcileBudget; best-effort, logged.
func maintainSharedServices(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("shared services upkeep: load state failed", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, sweep := range sharedServiceSweeps(st) {
		m, ok := sweep.runtime.(svcruntime.Maintainer)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, sweep.runtime.ReconcileBudget())
			defer cancel()
			if !sweep.runtime.Reachable(ctx) {
				return
			}
			for _, f := range sweep.fleets {
				if ctx.Err() != nil {
					return
				}
				if len(f.services) == 0 {
					continue
				}
				if err := m.Maintain(ctx, f.name, f.services); err != nil {
					flog.Warn("shared services upkeep failed", "runtime", sweep.runtime.Key(), "fleet", f.name, "err", err)
				}
			}
		}()
	}
	wg.Wait()
}
