package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDeleteBuildkitCacheRPC verifies the handler validates its input + the
// fleet's buildkit state, then delegates to the cache-wipe seam.
func TestDeleteBuildkitCacheRPC(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{BuildkitServer: true}},
		"beta":  {Name: "beta"}, // buildkit disabled
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var gotFleet string
	orig := deleteBuildkitCache
	deleteBuildkitCache = func(fleet string) error { gotFleet = fleet; return nil }
	t.Cleanup(func() { deleteBuildkitCache = orig })

	svc := newService()
	ctx := context.Background()

	// Happy path: enabled fleet → seam runs.
	if _, err := svc.DeleteBuildkitCache(ctx, &fleetgrpc.DeleteBuildkitCacheRequest{Fleet: "alpha"}); err != nil {
		t.Fatalf("DeleteBuildkitCache(alpha): %v", err)
	}
	if gotFleet != "alpha" {
		t.Fatalf("seam called with %q, want alpha", gotFleet)
	}

	// Empty fleet → InvalidArgument, seam not called.
	gotFleet = ""
	if _, err := svc.DeleteBuildkitCache(ctx, &fleetgrpc.DeleteBuildkitCacheRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty fleet: want InvalidArgument, got %v", err)
	}

	// Unknown fleet → NotFound, seam not called.
	if _, err := svc.DeleteBuildkitCache(ctx, &fleetgrpc.DeleteBuildkitCacheRequest{Fleet: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown fleet: want NotFound, got %v", err)
	}

	// Fleet without buildkit enabled → FailedPrecondition (must NOT spin up a
	// server via the restart step).
	if _, err := svc.DeleteBuildkitCache(ctx, &fleetgrpc.DeleteBuildkitCacheRequest{Fleet: "beta"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled fleet: want FailedPrecondition, got %v", err)
	}

	if gotFleet != "" {
		t.Fatalf("seam should not run for invalid/disabled fleets, ran for %q", gotFleet)
	}
}
