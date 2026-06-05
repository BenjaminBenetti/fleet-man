package server

import (
	"context"
	"os"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// stubStopBuildkit swaps the buildkit teardown seam for a recorder so the
// cleanup paths can be exercised without a docker daemon. Returns a pointer to
// the "was called" flag.
func stubStopBuildkit(t *testing.T) *bool {
	t.Helper()
	called := false
	orig := stopBuildkitServer
	stopBuildkitServer = func(string) error { called = true; return nil }
	t.Cleanup(func() { stopBuildkitServer = orig })
	return &called
}

func seedBuildkitFleet(t *testing.T, instances ...*fleet.Instance) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}, Instances: instances},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.MkdirAll(state.BuildkitDir("alpha"), 0o777); err != nil {
		t.Fatalf("mkdir buildkit dir: %v", err)
	}
}

// TestDestroyInstanceFleetTearsDownBuildkit verifies that destroying the whole
// fleet stops the shared server and removes its .buildkit directory.
func TestDestroyInstanceFleetTearsDownBuildkit(t *testing.T) {
	isolateFleetDir(t)
	called := stubStopBuildkit(t)
	seedBuildkitFleet(t, &fleet.Instance{Name: "i1"})

	newService().destroy("alpha", "i1", true)

	if !*called {
		t.Fatalf("StopSharedServer not called on destroy_fleet")
	}
	if _, err := os.Stat(state.BuildkitDir("alpha")); !os.IsNotExist(err) {
		t.Fatalf("buildkit dir not removed on destroy_fleet: %v", err)
	}
}

// TestDestroyInstanceSingleLeavesBuildkit verifies a single-instance destroy
// leaves the shared server (other instances may still use it) up and its dir
// intact.
func TestDestroyInstanceSingleLeavesBuildkit(t *testing.T) {
	isolateFleetDir(t)
	called := stubStopBuildkit(t)
	seedBuildkitFleet(t, &fleet.Instance{Name: "i1"}, &fleet.Instance{Name: "i2"})

	newService().destroy("alpha", "i1", false)

	if *called {
		t.Fatalf("StopSharedServer must NOT be called on a single-instance destroy")
	}
	if _, err := os.Stat(state.BuildkitDir("alpha")); err != nil {
		t.Fatalf("buildkit dir wrongly removed on single-instance destroy: %v", err)
	}
}

// TestDestroyFleetTearsDownBuildkit verifies the synchronous DestroyFleet RPC
// (used by the TUI to remove an already-empty fleet) also reclaims the buildkit
// server — previously it bypassed the job-based destroy() cleanup.
func TestDestroyFleetTearsDownBuildkit(t *testing.T) {
	isolateFleetDir(t)
	called := stubStopBuildkit(t)
	seedBuildkitFleet(t) // empty fleet

	if _, err := newService().DestroyFleet(context.Background(), &fleetgrpc.DestroyFleetRequest{Name: "alpha"}); err != nil {
		t.Fatalf("DestroyFleet: %v", err)
	}

	if !*called {
		t.Fatalf("StopSharedServer not called on DestroyFleet")
	}
	if _, err := os.Stat(state.BuildkitDir("alpha")); !os.IsNotExist(err) {
		t.Fatalf("buildkit dir not removed on DestroyFleet: %v", err)
	}
}
