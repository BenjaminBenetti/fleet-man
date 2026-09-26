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
