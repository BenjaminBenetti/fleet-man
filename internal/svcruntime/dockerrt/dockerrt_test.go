package dockerrt

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestEveryServiceRunsOnDocker: each shared service has its ensure, stop and
// wipe here, so a new service can't reach the runtime unwired.
func TestEveryServiceRunsOnDocker(t *testing.T) {
	for _, svc := range fleet.SharedServices() {
		s, ok := services[svc]
		if !ok || s.ensure == nil || s.stop == nil || s.wipe == nil {
			t.Errorf("%q: docker runtime entry %+v is incomplete", svc, s)
		}
	}
	if len(services) != len(fleet.SharedServices()) {
		t.Errorf("services has %d entries for %d shared services", len(services), len(fleet.SharedServices()))
	}
}

// recording replaces every docker call with a recorder of "<op> <fleet>
// [<service>]" lines.
func recording(t *testing.T, fail error) *[]string {
	t.Helper()
	var calls []string
	record := func(line string) error {
		calls = append(calls, line)
		return fail
	}
	orig, origConnect, origRemove := services, connectInstance, removeNetwork
	services = map[fleet.SharedService]service{}
	for svc, s := range orig {
		services[svc] = service{
			ensure:   func(f string) (string, error) { return "/dir", record("ensure " + f + " " + string(svc)) },
			stop:     func(f string) error { return record("stop " + f + " " + string(svc)) },
			wipe:     func(f string) error { return record("wipe " + f + " " + string(svc)) },
			mounts:   s.mounts,
			endpoint: s.endpoint,
		}
	}
	connectInstance = func(f, cid string) error { return record("connect " + f + " " + cid) }
	removeNetwork = func(f string) error { return record("remove-network " + f) }
	t.Cleanup(func() { services, connectInstance, removeNetwork = orig, origConnect, origRemove })
	return &calls
}

// TestDockerRuntimeCallsTheServicePackages: each method reaches the matching
// operation of the service's package, and passes its error on.
func TestDockerRuntimeCallsTheServicePackages(t *testing.T) {
	for _, fail := range []error{nil, errors.New("boom")} {
		calls := recording(t, fail)
		var rt Runtime
		var errs []error
		for _, svc := range fleet.SharedServices() {
			errs = append(errs, rt.EnsureService("alpha", svc), rt.StopService("alpha", svc), rt.WipeService("alpha", svc))
		}
		errs = append(errs, rt.ConnectInstance("alpha", "cid"), rt.TeardownFleet("alpha"))
		// Preparing and releasing a fleet touch nothing on docker: its
		// network comes with the first service, and its services stay until
		// the fleet goes.
		if err := rt.PrepareFleet("alpha"); err != nil {
			t.Errorf("PrepareFleet: %v", err)
		}
		if err := rt.ReleaseFleet("alpha"); err != nil {
			t.Errorf("ReleaseFleet: %v", err)
		}

		want := []string{
			"ensure alpha buildkit", "stop alpha buildkit", "wipe alpha buildkit",
			"ensure alpha debcache", "stop alpha debcache", "wipe alpha debcache",
			"ensure alpha imagecache", "stop alpha imagecache", "wipe alpha imagecache",
			"connect alpha cid", "remove-network alpha",
		}
		if !reflect.DeepEqual(*calls, want) {
			t.Fatalf("calls = %q, want %q", *calls, want)
		}
		for i, err := range errs {
			if !errors.Is(err, fail) {
				t.Errorf("call %d (%s) returned %v, want %v", i, want[i], err, fail)
			}
		}
	}
}

// TestInstanceMountsBindTheBuildkitSocketDir: buildkit is reached through its
// bind-mounted socket directory; the caches need no mount.
func TestInstanceMountsBindTheBuildkitSocketDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var rt Runtime
	want := []backend.Mount{buildkit.InstanceMount("alpha")}
	if got := rt.InstanceMounts("alpha", fleet.ServiceBuildkit); !reflect.DeepEqual(got, want) {
		t.Fatalf("buildkit mounts = %+v, want %+v", got, want)
	}
	if got := want[0].LocalPath; got != state.BuildkitDir("alpha") {
		t.Fatalf("buildkit mount source = %q, want the fleet's .buildkit dir", got)
	}
	for _, svc := range []fleet.SharedService{fleet.ServiceDebCache, fleet.ServiceImageCache, "unknown"} {
		if got := rt.InstanceMounts("alpha", svc); got != nil {
			t.Errorf("%q mounts = %+v, want none", svc, got)
		}
	}
}

// TestDockerEndpointsAreTheContainerNames: instances reach the caches by
// container name over the fleet network, and buildkit through its mount.
func TestDockerEndpointsAreTheContainerNames(t *testing.T) {
	var rt Runtime
	for svc, want := range map[fleet.SharedService]fleet.ServiceEndpoint{
		fleet.ServiceBuildkit:   {},
		fleet.ServiceDebCache:   {URL: debcache.ProxyURL("alpha")},
		fleet.ServiceImageCache: {URL: imagecache.MirrorURL("alpha"), HostPort: imagecache.MirrorHostPort("alpha")},
	} {
		got, err := rt.Endpoint("alpha", svc)
		if err != nil || got != want {
			t.Errorf("Endpoint(%q) = %+v, %v; want %+v", svc, got, err, want)
		}
	}
	if _, err := rt.Endpoint("alpha", "unknown"); err == nil {
		t.Error("an unknown service has an endpoint")
	}
}

func TestDockerRuntimeAnswers(t *testing.T) {
	var rt Runtime
	if rt.Key() != Key || Key != "docker" {
		t.Errorf("Key() = %q", rt.Key())
	}
	if rt.ReconcileBudget() != 3*time.Minute {
		t.Errorf("ReconcileBudget() = %v, want the 3 minutes a sweep has always had", rt.ReconcileBudget())
	}
	if !rt.ReconfigureOnStart() {
		t.Error("ReconfigureOnStart() = false: a started devcontainer would lose its caches after a host reboot")
	}
	if !rt.Reachable(context.Background()) {
		t.Error("Reachable() = false")
	}
}

func TestDockerRuntimeRefusesAnUnknownService(t *testing.T) {
	calls := recording(t, nil)
	var rt Runtime
	for name, err := range map[string]error{
		"ensure": rt.EnsureService("alpha", "unknown"),
		"stop":   rt.StopService("alpha", "unknown"),
		"wipe":   rt.WipeService("alpha", "unknown"),
	} {
		if err == nil {
			t.Errorf("%s of an unknown service succeeded", name)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("an unknown service reached docker: %q", *calls)
	}
}
