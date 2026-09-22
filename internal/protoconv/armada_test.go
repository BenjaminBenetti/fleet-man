package protoconv

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestArmadaRemoteRoundTripAllFields(t *testing.T) {
	in := []configutil.ArmadaRemote{filled[configutil.ArmadaRemote](t)}
	requireEqual(t, ArmadaRemotesFromProto(ArmadaRemotesToProto(in)), in)
}

func TestArmadaRemoteWirePlacement(t *testing.T) {
	pinArity[state.ArmadaRemote](t, 3)

	out := ArmadaRemotesToProto([]configutil.ArmadaRemote{{URL: "ssh://u@h", Token: "tk", ForwardAgent: true}})
	if len(out) != 1 || out[0].GetUrl() != "ssh://u@h" || out[0].GetToken() != "tk" || !out[0].GetForwardAgent() {
		t.Fatalf("armada remote wire placement wrong: %+v", out)
	}
}
