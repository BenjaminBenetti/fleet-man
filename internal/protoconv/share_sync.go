package protoconv

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// ShareUsageToProto maps a fleet's share on one connection to the wire.
func ShareUsageToProto(u fleet.ShareUsage) *fleetgrpc.KubeShareUsage {
	return &fleetgrpc.KubeShareUsage{
		Cluster: u.Cluster, Location: u.Location, Fleet: u.Fleet, Claim: u.Claim,
		UsedBytes: u.UsedBytes, CapacityBytes: u.CapacityBytes, Measured: u.Measured,
		Expandable: u.Expandable, Warning: u.Warning,
	}
}

// ShareUsageFromProto is the inverse of ShareUsageToProto.
func ShareUsageFromProto(p *fleetgrpc.KubeShareUsage) fleet.ShareUsage {
	return fleet.ShareUsage{
		Cluster: p.GetCluster(), Location: p.GetLocation(), Fleet: p.GetFleet(), Claim: p.GetClaim(),
		UsedBytes: p.GetUsedBytes(), CapacityBytes: p.GetCapacityBytes(), Measured: p.GetMeasured(),
		Expandable: p.GetExpandable(), Warning: p.GetWarning(),
	}
}

// ShareSyncToProto maps a fleet's sync to the wire.
func ShareSyncToProto(s fleet.ShareSync) *fleetgrpc.FleetShareSync {
	out := &fleetgrpc.FleetShareSync{Fleet: s.Fleet, Paused: cloneStrings(s.Paused), Conflicts: int32(s.Conflicts)}
	for _, r := range s.Roots {
		pr := &fleetgrpc.ShareSyncRoot{
			Root: r.Root, Files: int32(r.Files), Bytes: r.Bytes, Cap: r.Cap,
			Paused: r.Paused, Broken: r.Broken, Warnings: cloneStrings(r.Warnings),
		}
		for _, side := range r.Sides {
			pr.Sides = append(pr.Sides, &fleetgrpc.ShareSyncSide{
				Side: side.Side, Location: side.Location, Pending: int32(side.Pending), PendingBytes: side.PendingBytes,
				Paused: side.Paused, Refused: cloneStrings(side.Refused), LastError: side.LastError,
			})
		}
		out.Roots = append(out.Roots, pr)
	}
	for _, u := range s.Shares {
		out.Shares = append(out.Shares, ShareUsageToProto(u))
	}
	return out
}

// ShareSyncFromProto is the inverse of ShareSyncToProto.
func ShareSyncFromProto(p *fleetgrpc.FleetShareSync) fleet.ShareSync {
	out := fleet.ShareSync{Fleet: p.GetFleet(), Paused: cloneStrings(p.GetPaused()), Conflicts: int(p.GetConflicts())}
	for _, r := range p.GetRoots() {
		dr := fleet.ShareSyncRoot{
			Root: r.GetRoot(), Files: int(r.GetFiles()), Bytes: r.GetBytes(), Cap: r.GetCap(),
			Paused: r.GetPaused(), Broken: r.GetBroken(), Warnings: cloneStrings(r.GetWarnings()),
		}
		for _, side := range r.GetSides() {
			dr.Sides = append(dr.Sides, fleet.ShareSyncSide{
				Side: side.GetSide(), Location: side.GetLocation(), Pending: int(side.GetPending()), PendingBytes: side.GetPendingBytes(),
				Paused: side.GetPaused(), Refused: cloneStrings(side.GetRefused()), LastError: side.GetLastError(),
			})
		}
		out.Roots = append(out.Roots, dr)
	}
	for _, u := range p.GetShares() {
		out.Shares = append(out.Shares, ShareUsageFromProto(u))
	}
	return out
}

// ShareConflictToProto maps a conflict copy to the wire.
func ShareConflictToProto(c fleet.ShareConflict) *fleetgrpc.ShareConflict {
	out := &fleetgrpc.ShareConflict{
		Id: c.ID, Fleet: c.Fleet, Root: c.Root, Path: c.Path, Location: c.Location,
		Side: c.Side, Reason: c.Reason, Size: c.Size, File: c.File,
	}
	if !c.Time.IsZero() {
		out.Time = timestamppb.New(c.Time)
	}
	return out
}

// ShareConflictFromProto is the inverse of ShareConflictToProto.
func ShareConflictFromProto(p *fleetgrpc.ShareConflict) fleet.ShareConflict {
	out := fleet.ShareConflict{
		ID: p.GetId(), Fleet: p.GetFleet(), Root: p.GetRoot(), Path: p.GetPath(), Location: p.GetLocation(),
		Side: p.GetSide(), Reason: p.GetReason(), Size: p.GetSize(), File: p.GetFile(),
	}
	if t := p.GetTime(); t != nil {
		out.Time = t.AsTime()
	}
	return out
}

// cloneStrings copies a list, keeping nil nil.
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}
