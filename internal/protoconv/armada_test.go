package protoconv

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
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

func TestEditorEndpointRoundTripAllFields(t *testing.T) {
	in := filled[configutil.EditorEndpoint](t)
	requireEqual(t, EditorEndpointFromProto(EditorEndpointToProto(in)), in)
}

func TestEditorEndpointWirePlacement(t *testing.T) {
	pinArity[state.EditorEndpoint](t, 3)

	out := EditorEndpointToProto(configutil.EditorEndpoint{Kind: configutil.EditorEndpointSSH, Address: "ssh://u@h", Token: "tk"})
	if out.GetKind() != fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SSH || out.GetAddress() != "ssh://u@h" || out.GetToken() != "tk" {
		t.Fatalf("editor endpoint wire placement wrong: %+v", out)
	}
}

func TestEditorEndpointKindsMatchTheWire(t *testing.T) {
	for _, k := range []configutil.EditorEndpointKind{configutil.EditorEndpointGateway, configutil.EditorEndpointSSH, configutil.EditorEndpointServer} {
		if got := EditorEndpointKindFromProto(EditorEndpointKindToProto(k)); got != k {
			t.Errorf("kind %q round-trips to %q", k, got)
		}
	}
	// Every wire value but UNSPECIFIED names a kind.
	for v := range fleetgrpc.EditorEndpointKind_name {
		k := fleetgrpc.EditorEndpointKind(v)
		if k == fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_UNSPECIFIED {
			continue
		}
		if !EditorEndpointKindFromProto(k).Valid() {
			t.Errorf("wire kind %s maps to no kind", k)
		}
	}
	if got := EditorEndpointKindToProto("bogus"); got != fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_UNSPECIFIED {
		t.Errorf("an unknown kind went on the wire as %s", got)
	}
}
