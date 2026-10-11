package protoconv

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestShareUsageRoundTripAllFields(t *testing.T) {
	in := filled[fleet.ShareUsage](t)
	requireEqual(t, ShareUsageFromProto(ShareUsageToProto(in)), in)
}

func TestShareSyncRoundTripAllFields(t *testing.T) {
	in := filled[fleet.ShareSync](t)
	requireEqual(t, ShareSyncFromProto(ShareSyncToProto(in)), in)
}

func TestShareConflictRoundTripAllFields(t *testing.T) {
	in := filled[fleet.ShareConflict](t)
	requireEqual(t, ShareConflictFromProto(ShareConflictToProto(in)), in)
}

// An empty sync and a conflict without a time stay empty on the wire and
// back.
func TestShareSyncEmpty(t *testing.T) {
	if got := ShareSyncFromProto(ShareSyncToProto(fleet.ShareSync{Fleet: "f"})); got.Paused != nil || got.Roots != nil || got.Shares != nil {
		t.Fatalf("an empty sync round-trips to %+v", got)
	}
	if p := ShareConflictToProto(fleet.ShareConflict{ID: "x"}); p.GetTime() != nil {
		t.Fatalf("a conflict without a time has one on the wire: %v", p.GetTime())
	}
	if got := ShareConflictFromProto(&fleetgrpc.ShareConflict{}); !got.Time.IsZero() {
		t.Fatalf("a conflict without a time comes back with %v", got.Time)
	}
}
