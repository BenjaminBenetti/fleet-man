package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// stubEnsureBuildkit swaps the re-ensure seam for recordFn and restores it.
func stubEnsureBuildkit(t *testing.T, recordFn func(string) (string, error)) {
	t.Helper()
	orig := ensureBuildkitServer
	ensureBuildkitServer = recordFn
	t.Cleanup(func() { ensureBuildkitServer = orig })
}

// TestEnsureConfiguredBuildkitServersFiltering verifies the reconcile only
// ensures fleets that have the setting on AND at least one devcontainer
// instance — skipping disabled, empty, and cloud-only fleets.
func TestEnsureConfiguredBuildkitServersFiltering(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"enabled-dev": {Name: "enabled-dev", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
		"enabled-empty": {Name: "enabled-empty", Settings: fleet.FleetSettings{BuildkitServer: true}},
		"enabled-cloud": {Name: "enabled-cloud", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendCodespaces}}},
		"disabled": {Name: "disabled", Settings: fleet.FleetSettings{BuildkitServer: false},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var mu sync.Mutex
	var ensured []string
	stubEnsureBuildkit(t, func(name string) (string, error) {
		mu.Lock()
		ensured = append(ensured, name)
		mu.Unlock()
		return "", nil
	})

	ensureConfiguredBuildkitServers(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(ensured) != 1 || ensured[0] != "enabled-dev" {
		t.Fatalf("ensured = %v, want exactly [enabled-dev]", ensured)
	}
}

// TestEnsureConfiguredBuildkitServersFiltersAllOut is the zero-match boundary:
// only disabled / empty / cloud-only fleets → nothing is ensured.
func TestEnsureConfiguredBuildkitServersFiltersAllOut(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"disabled":      {Name: "disabled", Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
		"enabled-empty": {Name: "enabled-empty", Settings: fleet.FleetSettings{BuildkitServer: true}},
		"enabled-cloud": {Name: "enabled-cloud", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendCodespaces}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var calls atomic.Int32
	stubEnsureBuildkit(t, func(string) (string, error) { calls.Add(1); return "", nil })

	ensureConfiguredBuildkitServers(context.Background())

	if calls.Load() != 0 {
		t.Fatalf("ensure called %d times, want 0 (all fleets filtered)", calls.Load())
	}
}

// TestEnsureConfiguredBuildkitServersHandlesLoadError verifies a corrupt
// state.json is tolerated: the reconcile returns without panicking and without
// ensuring anything.
func TestEnsureConfiguredBuildkitServersHandlesLoadError(t *testing.T) {
	isolateFleetDir(t)
	if err := os.MkdirAll(filepath.Dir(state.StatePath()), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(state.StatePath(), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	var calls atomic.Int32
	stubEnsureBuildkit(t, func(string) (string, error) { calls.Add(1); return "", nil })

	ensureConfiguredBuildkitServers(context.Background()) // must not panic

	if calls.Load() != 0 {
		t.Fatalf("ensure called %d times on load error, want 0", calls.Load())
	}
}

// TestFleetTUIConnectedCoalesces verifies that concurrent TUI connects run the
// reconcile only once while one is in flight (multiple TUIs opening at once).
func TestFleetTUIConnectedCoalesces(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var count atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	stubEnsureBuildkit(t, func(name string) (string, error) {
		count.Add(1)
		started <- struct{}{}
		<-release
		return "", nil
	})

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
	for svc.buildkitReconciling.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.buildkitReconciling.Load() {
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
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var called atomic.Bool
	done := make(chan struct{})
	stubEnsureBuildkit(t, func(name string) (string, error) {
		called.Store(true)
		close(done)
		return "", nil
	})

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
	if !called.Load() {
		t.Fatal("ensure seam not invoked")
	}
}
