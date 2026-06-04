package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// coder.go implements the Coder-template-parameter read the TUI's settings
// dialog needs. The server owns all backend access (the P5 boundary), so the
// thin client asks for a template's rich parameters + preset names over RPC
// rather than calling internal/backend/coder's REST helpers directly.

// GetCoderTemplateParams resolves the template's active version, then fetches
// its rich parameters and presets. Presets are best-effort (a template without
// presets is normal), mirroring the old fetchCoderParamsCmd which ignored the
// presets error.
func (s *service) GetCoderTemplateParams(_ context.Context, req *fleetgrpc.GetCoderTemplateParamsRequest) (*fleetgrpc.GetCoderTemplateParamsReply, error) {
	versionID, err := coderbackend.FetchActiveVersionID(req.GetTemplate())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve coder template version: %v", err)
	}

	params, err := coderbackend.FetchRichParameters(versionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch coder rich parameters: %v", err)
	}

	reply := &fleetgrpc.GetCoderTemplateParamsReply{}
	for _, p := range params {
		reply.Parameters = append(reply.Parameters, &fleetgrpc.CoderRichParameter{
			Name:         p.Name,
			DisplayName:  p.DisplayName,
			Description:  p.Description,
			Type:         p.Type,
			DefaultValue: p.DefaultValue,
		})
	}

	// Presets are optional; a fetch error just means "no presets".
	presets, _ := coderbackend.FetchPresets(versionID)
	for _, preset := range presets {
		reply.Presets = append(reply.Presets, preset.Name)
	}

	return reply, nil
}
