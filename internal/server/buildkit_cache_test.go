package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDeleteBuildkitCacheRPC verifies the handler delegates to the cache-wipe
// seam and validates its input.
func TestDeleteBuildkitCacheRPC(t *testing.T) {
	var gotFleet string
	orig := deleteBuildkitCache
	deleteBuildkitCache = func(fleet string) error { gotFleet = fleet; return nil }
	t.Cleanup(func() { deleteBuildkitCache = orig })

	svc := newService()
	if _, err := svc.DeleteBuildkitCache(context.Background(), &fleetgrpc.DeleteBuildkitCacheRequest{Fleet: "alpha"}); err != nil {
		t.Fatalf("DeleteBuildkitCache: %v", err)
	}
	if gotFleet != "alpha" {
		t.Fatalf("seam called with %q, want alpha", gotFleet)
	}

	// Empty fleet -> InvalidArgument, seam not called.
	gotFleet = ""
	if _, err := svc.DeleteBuildkitCache(context.Background(), &fleetgrpc.DeleteBuildkitCacheRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty fleet: want InvalidArgument, got %v", err)
	}
	if gotFleet != "" {
		t.Fatalf("seam should not run for an empty fleet")
	}
}
