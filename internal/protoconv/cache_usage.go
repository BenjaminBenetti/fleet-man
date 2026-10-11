package protoconv

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// CacheUsageToProto maps one cache volume's usage to the wire.
func CacheUsageToProto(u fleet.CacheUsage) *fleetgrpc.CacheUsage {
	return &fleetgrpc.CacheUsage{
		Service:       string(u.Service),
		Cluster:       u.Cluster,
		Location:      u.Location,
		UsedBytes:     u.UsedBytes,
		CapacityBytes: u.CapacityBytes,
		Expandable:    u.Expandable,
		State:         u.State,
		Warning:       u.Warning,
	}
}

// CacheUsageFromProto is the inverse of CacheUsageToProto.
func CacheUsageFromProto(pu *fleetgrpc.CacheUsage) fleet.CacheUsage {
	return fleet.CacheUsage{
		Service:       fleet.SharedService(pu.GetService()),
		Cluster:       pu.GetCluster(),
		Location:      pu.GetLocation(),
		UsedBytes:     pu.GetUsedBytes(),
		CapacityBytes: pu.GetCapacityBytes(),
		Expandable:    pu.GetExpandable(),
		State:         pu.GetState(),
		Warning:       pu.GetWarning(),
	}
}

// CacheUsagesFromProto maps a GetCacheUsage reply's volumes.
func CacheUsagesFromProto(in []*fleetgrpc.CacheUsage) []fleet.CacheUsage {
	out := make([]fleet.CacheUsage, 0, len(in))
	for _, pu := range in {
		out = append(out, CacheUsageFromProto(pu))
	}
	return out
}
