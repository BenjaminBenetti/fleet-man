package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/svcruntimetest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubInstanceServices makes sharedServicesFor answer as svcruntime.ForInstance
// does (which instances get shared services at all) but hand out rec in place
// of the docker runtime, so a sweep runs without docker.
func stubInstanceServices(t *testing.T, rec *svcruntimetest.Recorder) {
	t.Helper()
	stubServicesFor(t, func(inst *fleet.Instance) svcruntime.Runtime {
		if svcruntime.ForInstance(inst) == nil {
			return nil
		}
		return rec
	})
}

func stubServicesFor(t *testing.T, fn func(*fleet.Instance) svcruntime.Runtime) {
	t.Helper()
	orig := sharedServicesFor
	sharedServicesFor = fn
	t.Cleanup(func() { sharedServicesFor = orig })
}

// stubHostServices makes the daemon host's runtime (teardown, cache wipes) rec.
func stubHostServices(t *testing.T, rec *svcruntimetest.Recorder) {
	t.Helper()
	orig := hostSharedServices
	hostSharedServices = func() svcruntime.Runtime { return rec }
	t.Cleanup(func() { hostSharedServices = orig })
}

func seedFleets(t *testing.T, fleets map[string]*fleet.Fleet) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: fleets}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// ensured lists the "<fleet>/<service>" pairs rec was asked to ensure.
func ensured(rec *svcruntimetest.Recorder) []string {
	var got []string
	for _, c := range rec.Calls() {
		if c.Method == "EnsureService" {
			got = append(got, c.Fleet+"/"+string(c.Service))
		}
	}
	return got
}

func settingsWith(svc fleet.SharedService) fleet.FleetSettings {
	switch svc {
	case fleet.ServiceBuildkit:
		return fleet.FleetSettings{BuildkitServer: true}
	case fleet.ServiceDebCache:
		return fleet.FleetSettings{DebCacheServer: true}
	default:
		return fleet.FleetSettings{ImageCacheServer: true}
	}
}

// TestSharedServicesSweepFiltering verifies the reconcile only ensures a
// service for fleets that have it on AND at least one devcontainer instance —
// skipping disabled, empty, and cloud-only fleets — for every service.
func TestSharedServicesSweepFiltering(t *testing.T) {
	for _, svc := range fleet.SharedServices() {
		t.Run(string(svc), func(t *testing.T) {
			isolateFleetDir(t)
			on := settingsWith(svc)
			seedFleets(t, map[string]*fleet.Fleet{
				"enabled-dev": {Name: "enabled-dev", Settings: on,
					Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
				"enabled-legacy": {Name: "enabled-legacy", Settings: on,
					Instances: []*fleet.Instance{{Name: "i1"}}}, // "" = devcontainer
				"enabled-empty": {Name: "enabled-empty", Settings: on},
				"enabled-cloud": {Name: "enabled-cloud", Settings: on,
					Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendCodespaces}, {Name: "i2", Backend: fleet.BackendCoder}}},
				"disabled": {Name: "disabled",
					Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
				"malformed": {Name: "malformed", Settings: on, Instances: []*fleet.Instance{nil}},
			})
			rec := svcruntimetest.New("docker")
			stubInstanceServices(t, rec)

			ensureConfiguredSharedServices(context.Background())

			want := []string{"enabled-dev/" + string(svc), "enabled-legacy/" + string(svc)}
			if got := ensured(rec); !reflect.DeepEqual(got, want) {
				t.Fatalf("ensured = %v, want %v", got, want)
			}
		})
	}
}

// TestSharedServicesSweepFiltersAllOut is the zero-match boundary: only
// disabled / empty / cloud-only fleets → nothing is ensured, and no runtime is
// even asked whether it is reachable.
func TestSharedServicesSweepFiltersAllOut(t *testing.T) {
	isolateFleetDir(t)
	all := fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true}
	seedFleets(t, map[string]*fleet.Fleet{
		"disabled":      {Name: "disabled", Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
		"enabled-empty": {Name: "enabled-empty", Settings: all},
		"enabled-cloud": {Name: "enabled-cloud", Settings: all,
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendCodespaces}}},
	})
	rec := svcruntimetest.New("docker")
	stubInstanceServices(t, rec)

	ensureConfiguredSharedServices(context.Background())

	if calls := rec.Calls(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none (all fleets filtered)", calls)
	}
}

// TestSharedServicesSweepEnsuresEachServiceOnce: a fleet with every service on
// and several instances on one runtime gets each service ensured once, in
// bring-up order, after the runtime was found reachable.
func TestSharedServicesSweepEnsuresEachServiceOnce(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1"}, {Name: "i2"}, {Name: "i3", Backend: fleet.BackendDevcontainer}}},
	})
	rec := svcruntimetest.New("docker")
	stubInstanceServices(t, rec)

	ensureConfiguredSharedServices(context.Background())

	want := []svcruntimetest.Call{
		{Method: "Reachable"},
		{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceBuildkit},
		{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceDebCache},
		{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceImageCache},
	}
	if got := rec.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
}

// TestSharedServicesSweepRunsEachRuntimeOnItsOwn: a fleet whose instances use
// two runtimes gets its services ensured on both, each runtime sees only the
// fleets that use it, and the two are swept concurrently (each runtime's
// ensure waits for the other's, which a serial sweep would never get past).
func TestSharedServicesSweepRunsEachRuntimeOnItsOwn(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"mixed": {Name: "mixed", Settings: fleet.FleetSettings{DebCacheServer: true},
			Instances: []*fleet.Instance{{Name: "a", Backend: fleet.BackendDevcontainer}, {Name: "b", Backend: fleet.BackendCoder}}},
		"only-a": {Name: "only-a", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "a", Backend: fleet.BackendDevcontainer}}},
	})
	recA, recB := svcruntimetest.New("rt-a"), svcruntimetest.New("rt-b")
	aIn, bIn := make(chan struct{}), make(chan struct{})
	meet := func(mine, theirs chan struct{}) func(svcruntimetest.Call) {
		var once atomic.Bool
		return func(c svcruntimetest.Call) {
			if c.Method != "EnsureService" || !once.CompareAndSwap(false, true) {
				return
			}
			close(mine)
			select {
			case <-theirs:
			case <-time.After(5 * time.Second):
			}
		}
	}
	recA.OnCall, recB.OnCall = meet(aIn, bIn), meet(bIn, aIn)
	stubServicesFor(t, func(inst *fleet.Instance) svcruntime.Runtime {
		if inst.Backend == fleet.BackendCoder {
			return recB
		}
		return recA
	})

	start := time.Now()
	ensureConfiguredSharedServices(context.Background())
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("sweep took %v: the runtimes were swept one after the other", elapsed)
	}

	if got, want := ensured(recA), []string{"mixed/debcache", "only-a/buildkit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("runtime a ensured %v, want %v", got, want)
	}
	if got, want := ensured(recB), []string{"mixed/debcache"}; !reflect.DeepEqual(got, want) {
		t.Errorf("runtime b ensured %v, want %v", got, want)
	}
}

// TestSharedServicesSweepSkipsAnUnreachableRuntime: a runtime that isn't
// reachable is asked nothing more, and the others are swept as usual.
func TestSharedServicesSweepSkipsAnUnreachableRuntime(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"mixed": {Name: "mixed", Settings: fleet.FleetSettings{ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "a", Backend: fleet.BackendDevcontainer}, {Name: "b", Backend: fleet.BackendCoder}}},
	})
	up, down := svcruntimetest.New("up"), svcruntimetest.New("down")
	down.Unreachable = true
	stubServicesFor(t, func(inst *fleet.Instance) svcruntime.Runtime {
		if inst.Backend == fleet.BackendCoder {
			return down
		}
		return up
	})

	ensureConfiguredSharedServices(context.Background())

	if got := down.Calls(); !reflect.DeepEqual(got, []svcruntimetest.Call{{Method: "Reachable"}}) {
		t.Errorf("unreachable runtime calls = %+v, want only Reachable", got)
	}
	if got, want := ensured(up), []string{"mixed/imagecache"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reachable runtime ensured %v, want %v", got, want)
	}
}

// TestSharedServicesSweepStopsWhenTheContextEnds: an ensure failure is only
// logged and the sweep goes on, but once ctx ends no further ensure starts.
func TestSharedServicesSweepStopsWhenTheContextEnds(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1"}}},
	})
	rec := svcruntimetest.New("docker")
	rec.FailOn("EnsureService", errors.New("docker is down"))
	ctx, cancel := context.WithCancel(context.Background())
	rec.OnCall = func(c svcruntimetest.Call) {
		if c.Method == "EnsureService" && c.Service == fleet.ServiceDebCache {
			cancel()
		}
	}
	stubInstanceServices(t, rec)

	ensureConfiguredSharedServices(ctx)

	if got, want := ensured(rec), []string{"alpha/buildkit", "alpha/debcache"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ensured = %v, want %v (go on after a failure, stop once ctx ends)", got, want)
	}
}

// TestSharedServicesSweepHandlesLoadError verifies a corrupt state.json is
// tolerated: the reconcile returns without panicking and without ensuring
// anything.
func TestSharedServicesSweepHandlesLoadError(t *testing.T) {
	isolateFleetDir(t)
	if err := os.MkdirAll(filepath.Dir(state.StatePath()), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(state.StatePath(), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	rec := svcruntimetest.New("docker")
	stubInstanceServices(t, rec)

	ensureConfiguredSharedServices(context.Background()) // must not panic

	if calls := rec.Calls(); len(calls) != 0 {
		t.Fatalf("calls on load error = %+v, want none", calls)
	}
}

// TestFleetTUIConnectedCoalesces verifies that concurrent TUI connects run the
// reconcile only once while one is in flight (multiple TUIs opening at once).
func TestFleetTUIConnectedCoalesces(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
	})

	var count atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	rec := svcruntimetest.New("docker")
	rec.OnCall = func(c svcruntimetest.Call) {
		if c.Method != "EnsureService" {
			return
		}
		count.Add(1)
		started <- struct{}{}
		<-release
	}
	stubInstanceServices(t, rec)

	svc := newService()
	svc.onTUIConnected() // #1 — wins the CAS, sweep runs and blocks in ensure
	<-started            // ensure is now in flight, flag held

	svc.onTUIConnected() // #2 — coalesced (skipped)
	svc.onTUIConnected() // #3 — coalesced (skipped)

	if got := count.Load(); got != 1 {
		t.Fatalf("ensure called %d times while one was in flight, want 1 (coalescing broken)", got)
	}

	close(release) // let #1's sweep finish and reset the flag
	deadline := time.Now().Add(2 * time.Second)
	for svc.sharedServicesReconciling.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.sharedServicesReconciling.Load() {
		t.Fatal("reconcile flag never reset after the sweep finished")
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("ensure called %d times total, want 1", got)
	}

	// Re-arm: a connect AFTER the flag reset must trigger a fresh sweep (guards
	// against the flag-reset path regressing into a permanent lockout). `release`
	// is already closed, so this sweep's ensure returns immediately.
	svc.onTUIConnected()
	deadline = time.Now().Add(2 * time.Second)
	for count.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("re-arm: ensure called %d times after flag reset, want 2", got)
	}
}

// TestFleetTUIConnectedRPCReturnsReply verifies the RPC handler returns a
// non-nil reply without error and triggers the reconcile.
func TestFleetTUIConnectedRPCReturnsReply(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{DebCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
	})
	done := make(chan struct{})
	var once atomic.Bool
	rec := svcruntimetest.New("docker")
	rec.OnCall = func(c svcruntimetest.Call) {
		if c.Method == "EnsureService" && once.CompareAndSwap(false, true) {
			close(done)
		}
	}
	stubInstanceServices(t, rec)

	reply, err := newService().FleetTUIConnected(context.Background(), &fleetgrpc.FleetTUIConnectedRequest{})
	if err != nil {
		t.Fatalf("FleetTUIConnected: %v", err)
	}
	if reply == nil {
		t.Fatal("FleetTUIConnected returned nil reply")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile was not triggered by FleetTUIConnected")
	}
}

// TestDeleteCacheRPCs verifies each Delete cache handler validates its input +
// the fleet's setting, then wipes that service on the daemon host's runtime.
func TestDeleteCacheRPCs(t *testing.T) {
	rpcs := []struct {
		svc     fleet.SharedService
		setting string // how the disabled error names it
		cache   string // how a wipe failure names it
		call    func(s *service, fleet string) error
	}{
		{fleet.ServiceBuildkit, "buildkit server", "buildkit cache", func(s *service, f string) error {
			_, err := s.DeleteBuildkitCache(context.Background(), &fleetgrpc.DeleteBuildkitCacheRequest{Fleet: f})
			return err
		}},
		{fleet.ServiceDebCache, "deb cache", "deb cache", func(s *service, f string) error {
			_, err := s.DeleteDebCache(context.Background(), &fleetgrpc.DeleteDebCacheRequest{Fleet: f})
			return err
		}},
		{fleet.ServiceImageCache, "image cache", "image cache", func(s *service, f string) error {
			_, err := s.DeleteImageCache(context.Background(), &fleetgrpc.DeleteImageCacheRequest{Fleet: f})
			return err
		}},
	}
	for _, rpc := range rpcs {
		t.Run(string(rpc.svc), func(t *testing.T) {
			isolateFleetDir(t)
			seedFleets(t, map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Settings: settingsWith(rpc.svc)},
				"beta":  {Name: "beta"}, // disabled
			})
			rec := svcruntimetest.New("docker")
			stubHostServices(t, rec)
			svc := newService()

			// Happy path: enabled fleet → the service is wiped.
			if err := rpc.call(svc, "alpha"); err != nil {
				t.Fatalf("delete alpha's %s: %v", rpc.svc, err)
			}
			want := []svcruntimetest.Call{{Method: "WipeService", Fleet: "alpha", Service: rpc.svc}}
			if got := rec.Calls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls = %+v, want %+v", got, want)
			}

			rec.Reset()
			if err := rpc.call(svc, ""); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("empty fleet: want InvalidArgument, got %v", err)
			}
			if err := rpc.call(svc, "ghost"); status.Code(err) != codes.NotFound {
				t.Fatalf("unknown fleet: want NotFound, got %v", err)
			}
			// A fleet without the service on must NOT get a server spun up by the
			// wipe's restart step.
			err := rpc.call(svc, "beta")
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), `fleet "beta" does not have the `+rpc.setting+" enabled") {
				t.Fatalf("disabled fleet: want FailedPrecondition naming the %s, got %v", rpc.setting, err)
			}
			if calls := rec.Calls(); len(calls) != 0 {
				t.Fatalf("wiped for invalid/disabled fleets: %+v", calls)
			}

			rec.FailOn("WipeService", errors.New("boom"))
			err = rpc.call(svc, "alpha")
			if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "delete "+rpc.cache+": boom") {
				t.Fatalf("failed wipe: want Internal %q, got %v", "delete "+rpc.cache+": boom", err)
			}
		})
	}
}

// fleetTeardown is what a whole-fleet teardown asks of the host runtime, in
// order: each server (buildkit only when it is on), then the network.
func fleetTeardown(fleetName string, buildkit bool) []svcruntimetest.Call {
	var calls []svcruntimetest.Call
	if buildkit {
		calls = append(calls, svcruntimetest.Call{Method: "StopService", Fleet: fleetName, Service: fleet.ServiceBuildkit})
	}
	return append(calls,
		svcruntimetest.Call{Method: "StopService", Fleet: fleetName, Service: fleet.ServiceDebCache},
		svcruntimetest.Call{Method: "StopService", Fleet: fleetName, Service: fleet.ServiceImageCache},
		svcruntimetest.Call{Method: "TeardownFleet", Fleet: fleetName},
	)
}

// TestDestroyFleetStopsSharedServicesKeepsCaches verifies that destroying the
// whole fleet removes the shared servers and then the network, but PRESERVES
// the cache directories so a future fleet of the same name warms from them.
func TestDestroyFleetStopsSharedServicesKeepsCaches(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1"}}},
	})
	if err := os.MkdirAll(state.BuildkitDir("alpha"), 0o777); err != nil {
		t.Fatalf("mkdir buildkit dir: %v", err)
	}
	rec := svcruntimetest.New("docker")
	stubHostServices(t, rec)

	newService().destroy("alpha", "i1", true)

	if got, want := rec.Calls(), fleetTeardown("alpha", true); !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown calls = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(state.BuildkitDir("alpha")); err != nil {
		t.Fatalf("buildkit cache dir should persist across destroy_fleet, got: %v", err)
	}
}

// TestDestroyFleetTearsDownCachesUnconditionally guards the orphan-on-destroy
// fix: even when the cache settings are OFF at destroy time (a user disabled the
// toggle, which does NOT stop the running container), and whatever the fleet's
// instances ran on, a full fleet destroy must still tear down the cache
// containers and the shared network on the host. Buildkit follows its setting.
func TestDestroyFleetTearsDownCachesUnconditionally(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		// Caches toggled OFF, but instances exist.
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1"}, {Name: "i2", Backend: fleet.BackendCodespaces}}},
	})
	rec := svcruntimetest.New("docker")
	stubHostServices(t, rec)

	newService().destroy("alpha", "i1", true) // destroy_fleet

	if got, want := rec.Calls(), fleetTeardown("alpha", false); !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown calls = %+v, want %+v", got, want)
	}
}

// TestDestroySingleInstanceLeavesSharedServices verifies a single-instance
// destroy does NOT tear down the shared servers or network (the fleet's other
// instances may use them).
func TestDestroySingleInstanceLeavesSharedServices(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1"}, {Name: "i2"}}},
	})
	if err := os.MkdirAll(state.BuildkitDir("alpha"), 0o777); err != nil {
		t.Fatalf("mkdir buildkit dir: %v", err)
	}
	rec := svcruntimetest.New("docker")
	stubHostServices(t, rec)

	newService().destroy("alpha", "i1", false) // single-instance

	if calls := rec.Calls(); len(calls) != 0 {
		t.Fatalf("single-instance destroy touched the shared services: %+v", calls)
	}
	if _, err := os.Stat(state.BuildkitDir("alpha")); err != nil {
		t.Fatalf("buildkit dir wrongly removed on single-instance destroy: %v", err)
	}
}

// TestDestroyFleetRPCStopsSharedServices verifies the synchronous DestroyFleet
// RPC (used by the TUI to remove an already-empty fleet) also tears the shared
// services down — previously it bypassed the job-based cleanup and orphaned
// the buildkit server — while preserving the cache directory. A fleet that is
// already gone still gets the unconditional cache + network teardown.
func TestDestroyFleetRPCStopsSharedServices(t *testing.T) {
	isolateFleetDir(t)
	seedFleets(t, map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}}, // empty fleet
	})
	if err := os.MkdirAll(state.BuildkitDir("alpha"), 0o777); err != nil {
		t.Fatalf("mkdir buildkit dir: %v", err)
	}
	rec := svcruntimetest.New("docker")
	stubHostServices(t, rec)
	svc := newService()

	if _, err := svc.DestroyFleet(context.Background(), &fleetgrpc.DestroyFleetRequest{Name: "alpha"}); err != nil {
		t.Fatalf("DestroyFleet: %v", err)
	}
	if got, want := rec.Calls(), fleetTeardown("alpha", true); !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown calls = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(state.BuildkitDir("alpha")); err != nil {
		t.Fatalf("buildkit cache dir should persist across DestroyFleet, got: %v", err)
	}

	rec.Reset()
	if _, err := svc.DestroyFleet(context.Background(), &fleetgrpc.DestroyFleetRequest{Name: "alpha"}); err != nil {
		t.Fatalf("DestroyFleet of a gone fleet: %v", err)
	}
	if got, want := rec.Calls(), fleetTeardown("alpha", false); !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown calls for a gone fleet = %+v, want %+v", got, want)
	}
}

// TestTeardownFleetSharedServicesWarns: a failing step becomes a warning naming
// it, and the later steps still run.
func TestTeardownFleetSharedServicesWarns(t *testing.T) {
	rec := svcruntimetest.New("docker")
	rec.FailOn("StopService", errors.New("boom"))
	rec.FailOn("TeardownFleet", errors.New("in use"))
	stubHostServices(t, rec)

	got := teardownFleetSharedServices("alpha", true)

	want := []string{
		"stop buildkit server: boom",
		"stop deb cache server: boom",
		"stop image cache server: boom",
		"remove fleet network: in use",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings = %q, want %q", got, want)
	}
	if calls := rec.Calls(); !reflect.DeepEqual(calls, fleetTeardown("alpha", true)) {
		t.Fatalf("calls = %+v, want every step", calls)
	}
}

// TestSharedServiceTextNamesEveryService: every shared service has the names
// the daemon's messages use for it.
func TestSharedServiceTextNamesEveryService(t *testing.T) {
	for _, svc := range fleet.SharedServices() {
		text, ok := sharedServiceText[svc]
		if !ok || text.setting == "" || text.cache == "" || text.server == "" {
			t.Errorf("%q: sharedServiceText = %+v", svc, text)
		}
	}
}
