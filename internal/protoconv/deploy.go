package protoconv

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// DeployTargetToProto maps a deploy target to the proto contract. The zero
// (unspecified) target maps to nil.
func DeployTargetToProto(t fleet.DeployTarget) *fleetgrpc.DeployTarget {
	if t.IsZero() {
		return nil
	}
	return &fleetgrpc.DeployTarget{Backend: BackendToProto(t.Backend), KubeCluster: t.KubeCluster}
}

// DeployTargetFromProto is the inverse of DeployTargetToProto. nil, an
// UNSPECIFIED backend and one this build doesn't know are all the zero target,
// whatever cluster they name: a target this build can't name is never read
// as one it can.
func DeployTargetFromProto(pt *fleetgrpc.DeployTarget) fleet.DeployTarget {
	backend := BackendFromProto(pt.GetBackend())
	if backend == "" {
		return fleet.DeployTarget{}
	}
	return fleet.DeployTarget{Backend: backend, KubeCluster: pt.GetKubeCluster()}
}

// DeployTargetOptionsToProto maps ListDeployTargets entries to the proto
// contract.
func DeployTargetOptionsToProto(in []fleet.DeployTargetOption) []*fleetgrpc.DeployTargetOption {
	out := make([]*fleetgrpc.DeployTargetOption, 0, len(in))
	for _, o := range in {
		out = append(out, &fleetgrpc.DeployTargetOption{
			Target:    DeployTargetToProto(o.Target),
			Available: o.Available,
			Reason:    o.Reason,
			Warnings:  o.Warnings,
		})
	}
	return out
}

// DeployTargetOptionsFromProto is the inverse of DeployTargetOptionsToProto.
func DeployTargetOptionsFromProto(in []*fleetgrpc.DeployTargetOption) []fleet.DeployTargetOption {
	out := make([]fleet.DeployTargetOption, 0, len(in))
	for _, o := range in {
		out = append(out, fleet.DeployTargetOption{
			Target:    DeployTargetFromProto(o.GetTarget()),
			Available: o.GetAvailable(),
			Reason:    o.GetReason(),
			Warnings:  o.GetWarnings(),
		})
	}
	return out
}

// HostToolsToProto maps daemon-host tool statuses to the proto contract.
func HostToolsToProto(in []deps.ToolStatus) []*fleetgrpc.HostTool {
	out := make([]*fleetgrpc.HostTool, 0, len(in))
	for _, t := range in {
		out = append(out, &fleetgrpc.HostTool{
			Name:        t.Name,
			Binary:      t.Binary,
			Description: t.Description,
			InstallUrl:  t.InstallURL,
			Found:       t.Found,
		})
	}
	return out
}

// HostToolsFromProto is the inverse of HostToolsToProto.
func HostToolsFromProto(in []*fleetgrpc.HostTool) []deps.ToolStatus {
	out := make([]deps.ToolStatus, 0, len(in))
	for _, t := range in {
		out = append(out, deps.ToolStatus{
			Name:        t.GetName(),
			Binary:      t.GetBinary(),
			Description: t.GetDescription(),
			InstallURL:  t.GetInstallUrl(),
			Found:       t.GetFound(),
		})
	}
	return out
}
