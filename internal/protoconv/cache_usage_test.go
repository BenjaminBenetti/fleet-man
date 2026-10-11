package protoconv

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestCacheUsageRoundTripAllFields(t *testing.T) {
	in := filled[fleet.CacheUsage](t)
	requireEqual(t, CacheUsageFromProto(CacheUsageToProto(in)), in)
	requireEqual(t, CacheUsagesFromProto([]*fleetgrpc.CacheUsage{CacheUsageToProto(in)}), []fleet.CacheUsage{in})
}
