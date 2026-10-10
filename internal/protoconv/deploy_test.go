package protoconv

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestDeployTargetRoundTripAllFields(t *testing.T) {
	in := filled[fleet.DeployTarget](t)
	requireEqual(t, DeployTargetFromProto(DeployTargetToProto(in)), in)
}

func TestDeployTargetOptionRoundTripAllFields(t *testing.T) {
	in := []fleet.DeployTargetOption{filled[fleet.DeployTargetOption](t)}
	requireEqual(t, DeployTargetOptionsFromProto(DeployTargetOptionsToProto(in)), in)
}

func TestHostToolRoundTripAllFields(t *testing.T) {
	in := []deps.ToolStatus{filled[deps.ToolStatus](t)}
	requireEqual(t, HostToolsFromProto(HostToolsToProto(in)), in)
}

// The zero target is "unspecified": it travels as an absent message, and an
// absent or UNSPECIFIED message reads back as the zero target.
func TestDeployTargetUnspecified(t *testing.T) {
	if pt := DeployTargetToProto(fleet.DeployTarget{}); pt != nil {
		t.Fatalf("zero target = %v, want nil", pt)
	}
	if got := DeployTargetFromProto(nil); !got.IsZero() {
		t.Fatalf("nil target read back as %v", got)
	}
	if got := DeployTargetFromProto(&fleetgrpc.DeployTarget{}); !got.IsZero() {
		t.Fatalf("UNSPECIFIED target read back as %v", got)
	}
	// A backend this build can't name stays the zero target even with a
	// cluster, so the dialog never offers it as one it can.
	unknown := &fleetgrpc.DeployTarget{Backend: fleetgrpc.BackendType(99), KubeCluster: "kc"}
	if got := DeployTargetFromProto(unknown); !got.IsZero() {
		t.Fatalf("unknown backend target read back as %v", got)
	}
}

func TestDeployTargetWirePlacement(t *testing.T) {
	pinArity[fleet.DeployTarget](t, 2)
	pinArity[fleet.DeployTargetOption](t, 3)

	pt := DeployTargetToProto(fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "kc"})
	if pt.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES || pt.GetKubeCluster() != "kc" {
		t.Fatalf("deploy target wire placement wrong: %v", pt)
	}
	opts := DeployTargetOptionsToProto([]fleet.DeployTargetOption{{
		Target:    fleet.DeployTarget{Backend: fleet.BackendCoder},
		Available: true,
		Reason:    "why",
	}})
	if len(opts) != 1 || opts[0].GetTarget().GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODER || !opts[0].GetAvailable() || opts[0].GetReason() != "why" {
		t.Fatalf("deploy target option wire placement wrong: %v", opts)
	}
}

func TestHostToolWirePlacement(t *testing.T) {
	pinArity[deps.ToolStatus](t, 5)

	out := HostToolsToProto([]deps.ToolStatus{{Name: "n", Binary: "b", Description: "d", InstallURL: "u", Found: true}})
	if len(out) != 1 {
		t.Fatalf("got %d host tools", len(out))
	}
	tool := out[0]
	if tool.GetName() != "n" || tool.GetBinary() != "b" || tool.GetDescription() != "d" || tool.GetInstallUrl() != "u" || !tool.GetFound() {
		t.Fatalf("host tool wire placement wrong: %v", tool)
	}
}
