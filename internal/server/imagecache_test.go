package server

import (
	"context"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDeleteImageCacheRPC verifies the handler validates its input + the fleet's
// image-cache state, then delegates to the cache-wipe seam.
func TestDeleteImageCacheRPC(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{ImageCacheServer: true}},
		"beta":  {Name: "beta"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var gotFleet string
	orig := deleteImageCache
	deleteImageCache = func(fleet string) error { gotFleet = fleet; return nil }
	t.Cleanup(func() { deleteImageCache = orig })

	svc := newService()
	ctx := context.Background()

	if _, err := svc.DeleteImageCache(ctx, &fleetgrpc.DeleteImageCacheRequest{Fleet: "alpha"}); err != nil {
		t.Fatalf("DeleteImageCache(alpha): %v", err)
	}
	if gotFleet != "alpha" {
		t.Fatalf("seam called with %q, want alpha", gotFleet)
	}

	gotFleet = ""
	if _, err := svc.DeleteImageCache(ctx, &fleetgrpc.DeleteImageCacheRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty fleet: want InvalidArgument, got %v", err)
	}
	if _, err := svc.DeleteImageCache(ctx, &fleetgrpc.DeleteImageCacheRequest{Fleet: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown fleet: want NotFound, got %v", err)
	}
	if _, err := svc.DeleteImageCache(ctx, &fleetgrpc.DeleteImageCacheRequest{Fleet: "beta"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled fleet: want FailedPrecondition, got %v", err)
	}
	if gotFleet != "" {
		t.Fatalf("seam should not run for invalid/disabled fleets, ran for %q", gotFleet)
	}
}

// TestEnsureConfiguredImageCacheServersFiltering verifies the reconcile only
// ensures fleets that have the setting on AND a devcontainer instance.
func TestEnsureConfiguredImageCacheServersFiltering(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"enabled-dev": {Name: "enabled-dev", Settings: fleet.FleetSettings{ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
		"enabled-empty": {Name: "enabled-empty", Settings: fleet.FleetSettings{ImageCacheServer: true}},
		"enabled-cloud": {Name: "enabled-cloud", Settings: fleet.FleetSettings{ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendCodespaces}}},
		"disabled": {Name: "disabled", Settings: fleet.FleetSettings{ImageCacheServer: false},
			Instances: []*fleet.Instance{{Name: "i1", Backend: fleet.BackendDevcontainer}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var mu sync.Mutex
	var ensured []string
	orig := ensureImageCacheServer
	ensureImageCacheServer = func(name string) (string, error) {
		mu.Lock()
		ensured = append(ensured, name)
		mu.Unlock()
		return "", nil
	}
	t.Cleanup(func() { ensureImageCacheServer = orig })

	ensureConfiguredImageCacheServers(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(ensured) != 1 || ensured[0] != "enabled-dev" {
		t.Fatalf("ensured = %v, want exactly [enabled-dev]", ensured)
	}
}
