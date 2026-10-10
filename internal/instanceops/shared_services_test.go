package instanceops

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/svcruntimetest"
)

// stubStartServices makes the starting instance's shared-service runtime rt
// (nil: none) and records each image-mirror configure.
func stubStartServices(t *testing.T, rt svcruntime.Runtime) *[]string {
	t.Helper()
	origFor, origMirror := sharedServicesFor, configureImageMirror
	sharedServicesFor = func(*fleet.Instance) svcruntime.Runtime { return rt }
	var mirrored []string
	configureImageMirror = func(fleetName, instanceName string, instance *fleet.Instance) {
		mirrored = append(mirrored, fleetName+"/"+instanceName+"@"+instance.ContainerID)
	}
	t.Cleanup(func() { sharedServicesFor, configureImageMirror = origFor, origMirror })
	return &mirrored
}

func setLifecycleSettings(t *testing.T, settings fleet.FleetSettings) {
	t.Helper()
	if err := state.Update(func(st *state.State) error {
		st.Fleets["alpha"].Settings = settings
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStartRevivesSharedServices: a start re-ensures each enabled shared
// service and reconnects the instance to the network-based ones on a runtime
// that reconfigures on start, and points the instance's dockerd at the image
// cache on every start, whatever the runtime.
func TestStartRevivesSharedServices(t *testing.T) {
	all := fleet.FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true}
	tests := []struct {
		name        string
		settings    fleet.FleetSettings
		noRuntime   bool
		reconfigure bool
		want        []svcruntimetest.Call
		mirror      bool
	}{
		{
			name: "every service", settings: all, reconfigure: true,
			want: []svcruntimetest.Call{
				{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceBuildkit},
				{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceDebCache},
				{Method: "ConnectInstance", Fleet: "alpha", ContainerID: "abc123"},
				{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceImageCache},
				{Method: "ConnectInstance", Fleet: "alpha", ContainerID: "abc123"},
			},
			mirror: true,
		},
		{
			name: "buildkit only", settings: fleet.FleetSettings{BuildkitServer: true}, reconfigure: true,
			want: []svcruntimetest.Call{{Method: "EnsureService", Fleet: "alpha", Service: fleet.ServiceBuildkit}},
		},
		{name: "runtime whose services persist", settings: all, mirror: true},
		{name: "nothing on", reconfigure: true},
		{name: "no shared-service runtime", settings: all, noRuntime: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			restore := stubLifecycleClient(&fakeClient{})
			defer restore()
			writeLifecycleState(t, fleet.StatusStopped)
			setLifecycleSettings(t, tt.settings)
			rec := svcruntimetest.New("docker")
			rec.Reconfigure = tt.reconfigure
			var rt svcruntime.Runtime = rec
			if tt.noRuntime {
				rt = nil
			}
			mirrored := stubStartServices(t, rt)

			if _, err := StartInstance("alpha", "agent-1"); err != nil {
				t.Fatalf("StartInstance() error = %v", err)
			}

			if got := rec.Calls(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("runtime calls = %+v, want %+v", got, tt.want)
			}
			var wantMirror []string
			if tt.mirror {
				wantMirror = []string{"alpha/agent-1@abc123"}
			}
			if !reflect.DeepEqual(*mirrored, wantMirror) {
				t.Errorf("image mirror configured for %v, want %v", *mirrored, wantMirror)
			}
		})
	}
}

// TestStartWarnsAndGoesOnWhenSharedServicesFail: each failure is a warning on
// the instance, the later steps still run, and the instance still starts.
func TestStartWarnsAndGoesOnWhenSharedServicesFail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := stubLifecycleClient(&fakeClient{})
	defer restore()
	writeLifecycleState(t, fleet.StatusStopped)
	setLifecycleSettings(t, fleet.FleetSettings{DebCacheServer: true, ImageCacheServer: true})
	rec := svcruntimetest.New("docker")
	rec.FailOn("EnsureService", errors.New("no docker"))
	rec.FailOn("ConnectInstance", errors.New("no network"))
	mirrored := stubStartServices(t, rec)

	result, err := StartInstance("alpha", "agent-1")
	if err != nil {
		t.Fatalf("StartInstance() error = %v", err)
	}
	if result.Status != fleet.StatusRunning {
		t.Fatalf("status = %q, want running", result.Status)
	}
	if got := len(rec.Calls()); got != 4 {
		t.Fatalf("runtime calls = %+v, want ensure + connect for both caches", rec.Calls())
	}
	if len(*mirrored) != 1 {
		t.Fatalf("image mirror configured %d times, want 1", len(*mirrored))
	}
	// Each warning replaces the banner; the last step's is what is left.
	banner, err := os.ReadFile(state.WarnPath("alpha", "agent-1"))
	if err != nil || string(banner) != "image cache network: no network" {
		t.Fatalf("banner = %q (%v)", banner, err)
	}
}
