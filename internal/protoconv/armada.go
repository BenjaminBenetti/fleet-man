package protoconv

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
)

// ArmadaRemotesToProto maps the armada registry entries to the proto contract.
func ArmadaRemotesToProto(in []configutil.ArmadaRemote) []*fleetgrpc.ArmadaRemote {
	out := make([]*fleetgrpc.ArmadaRemote, 0, len(in))
	for _, r := range in {
		out = append(out, &fleetgrpc.ArmadaRemote{Url: r.URL, Token: r.Token, ForwardAgent: r.ForwardAgent})
	}
	return out
}

// ArmadaRemotesFromProto is the inverse of ArmadaRemotesToProto.
func ArmadaRemotesFromProto(in []*fleetgrpc.ArmadaRemote) []configutil.ArmadaRemote {
	out := make([]configutil.ArmadaRemote, 0, len(in))
	for _, r := range in {
		out = append(out, configutil.ArmadaRemote{URL: r.GetUrl(), Token: r.GetToken(), ForwardAgent: r.GetForwardAgent()})
	}
	return out
}

// EditorEndpointToProto maps an editor endpoint record to the proto contract.
func EditorEndpointToProto(in configutil.EditorEndpoint) *fleetgrpc.EditorEndpoint {
	return &fleetgrpc.EditorEndpoint{Kind: EditorEndpointKindToProto(in.Kind), Address: in.Address, Token: in.Token}
}

// EditorEndpointFromProto is the inverse of EditorEndpointToProto.
func EditorEndpointFromProto(in *fleetgrpc.EditorEndpoint) configutil.EditorEndpoint {
	return configutil.EditorEndpoint{Kind: EditorEndpointKindFromProto(in.GetKind()), Address: in.GetAddress(), Token: in.GetToken()}
}

// EditorEndpointKindToProto maps an endpoint kind; an unknown one is
// UNSPECIFIED.
func EditorEndpointKindToProto(k configutil.EditorEndpointKind) fleetgrpc.EditorEndpointKind {
	switch k {
	case configutil.EditorEndpointGateway:
		return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY
	case configutil.EditorEndpointSSH:
		return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SSH
	case configutil.EditorEndpointServer:
		return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SERVER
	}
	return fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_UNSPECIFIED
}

// EditorEndpointKindFromProto is the inverse of EditorEndpointKindToProto
// ("" for UNSPECIFIED).
func EditorEndpointKindFromProto(k fleetgrpc.EditorEndpointKind) configutil.EditorEndpointKind {
	switch k {
	case fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_GATEWAY:
		return configutil.EditorEndpointGateway
	case fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SSH:
		return configutil.EditorEndpointSSH
	case fleetgrpc.EditorEndpointKind_EDITOR_ENDPOINT_KIND_SERVER:
		return configutil.EditorEndpointServer
	}
	return ""
}
