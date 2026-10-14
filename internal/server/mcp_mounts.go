package server

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// mcp_mounts.go adds the custom-mount tools: list, add-or-change and remove a
// fleet's custom mounts, each optionally on a folder of the daemon host
// instead of its .mnt folder (the same settings the TUI's fleet options and
// `fleet mount` edit). Writes are read-modify-write of FleetSettings through
// mutateAutomation, the shared fleet.SetCustomMount / RemoveCustomMount
// owning the rules and SetFleetSettings checking them again.

// mcpMount is one custom mount.
type mcpMount struct {
	Path string `json:"path"`
	// HostFolder is the daemon-host folder it binds ("" for its .mnt
	// folder under ~/.fleet/workspaces/<fleet>).
	HostFolder string `json:"host_folder,omitempty"`
}

// MountsOutput is a fleet's custom mounts, returned by every mount tool.
type MountsOutput struct {
	Mounts []mcpMount `json:"mounts"`
	// Warnings are what the change was saved with but the caller should
	// know: a host folder missing on the daemon host.
	Warnings []string `json:"warnings,omitempty"`
}

func toMCPMounts(s fleet.FleetSettings, warnings []string) MountsOutput {
	out := MountsOutput{Mounts: []mcpMount{}, Warnings: warnings}
	for _, p := range s.CustomMounts {
		out.Mounts = append(out.Mounts, mcpMount{Path: p, HostFolder: s.CustomMountHostFolders[p]})
	}
	return out
}

type FleetMountListInput struct {
	Fleet string `json:"fleet" jsonschema:"fleet name"`
}

func (s *service) mcpMountList(ctx context.Context, _ *mcp.CallToolRequest, in FleetMountListInput) (*mcp.CallToolResult, MountsOutput, error) {
	if in.Fleet == "" {
		return nil, MountsOutput{}, errors.New("fleet is required")
	}
	settings, err := s.automationSettings(ctx, in.Fleet)
	if err != nil {
		return nil, MountsOutput{}, err
	}
	return nil, toMCPMounts(settings, nil), nil
}

type FleetMountSetInput struct {
	Fleet      string `json:"fleet" jsonschema:"fleet name"`
	Path       string `json:"path" jsonschema:"absolute path the mount appears at in every instance of the fleet, e.g. /opt/data"`
	HostFolder string `json:"host_folder,omitempty" jsonschema:"absolute folder on the daemon host to bind as it is (never made or chmodded; on a Kubernetes cluster it is synced both ways), instead of the fleet's .mnt folder. Omit to use (or go back to) the .mnt folder"`
}

func (s *service) mcpMountSet(ctx context.Context, _ *mcp.CallToolRequest, in FleetMountSetInput) (*mcp.CallToolResult, MountsOutput, error) {
	if in.Fleet == "" || in.Path == "" {
		return nil, MountsOutput{}, errors.New("fleet and path are required")
	}
	settings, warnings, err := s.mutateAutomation(ctx, in.Fleet, func(fs fleet.FleetSettings) (fleet.FleetSettings, error) {
		return fleet.SetCustomMount(fs, in.Path, in.HostFolder)
	})
	if err != nil {
		return nil, MountsOutput{}, err
	}
	return nil, toMCPMounts(settings, warnings), nil
}

type FleetMountDeleteInput struct {
	Fleet string `json:"fleet" jsonschema:"fleet name"`
	Path  string `json:"path" jsonschema:"the custom mount's path"`
}

func (s *service) mcpMountDelete(ctx context.Context, _ *mcp.CallToolRequest, in FleetMountDeleteInput) (*mcp.CallToolResult, MountsOutput, error) {
	if in.Fleet == "" || in.Path == "" {
		return nil, MountsOutput{}, errors.New("fleet and path are required")
	}
	settings, warnings, err := s.mutateAutomation(ctx, in.Fleet, func(fs fleet.FleetSettings) (fleet.FleetSettings, error) {
		return fleet.RemoveCustomMount(fs, in.Path)
	})
	if err != nil {
		return nil, MountsOutput{}, err
	}
	return nil, toMCPMounts(settings, warnings), nil
}
