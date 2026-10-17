package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// mcp_fleet_settings.go adds the fleet options tools: read (fleet_settings)
// and change (fleet_edit) every setting of the TUI's Edit fleet dialog, and
// the settings object fleet_up stores on a fleet it makes. The automation
// agents and triggers stored beside them have their own tools
// (mcp_automation.go). A change is read-modify-write of FleetSettings through
// mutateAutomation, so SetFleetSettings checks it like any other write.

// mcpFleetSettingsInput is a fleet's options, in the Edit fleet dialog's order.
// Every field is optional: fleet_up stores what is passed on the fleet it
// makes (the rest off/empty), fleet_edit changes only what is passed. A list
// replaces the whole list; [] empties it.
type mcpFleetSettingsInput struct {
	ClaudeCodeMount    *bool                    `json:"claude_code_mount,omitempty" jsonschema:"share one Claude Code login and state (~/.claude, ~/.claude.json) across the fleet's instances"`
	CodexMount         *bool                    `json:"codex_mount,omitempty" jsonschema:"share one Codex login and state (~/.codex) across the fleet's instances"`
	AuggieMount        *bool                    `json:"auggie_mount,omitempty" jsonschema:"share one Auggie (Augment CLI) login and state (~/.augment) across the fleet's instances, and install Auggie in them"`
	FleetMCP           *bool                    `json:"fleet_mcp,omitempty" jsonschema:"serve this full fleet MCP (every tool, every fleet) and the Fleet Admiral skill to the coding agents in the fleet's instances; it gives anything running in them host-level access. Devcontainer instances on a Linux host, and Kubernetes instances"`
	GhMount            *bool                    `json:"gh_mount,omitempty" jsonschema:"share one GitHub CLI login (~/.config/gh) across the fleet's instances"`
	HomeDir            *string                  `json:"home_dir,omitempty" jsonschema:"absolute home directory of the instances' user, where the shared logins are mounted, e.g. /home/node; empty for the default /home/vscode"`
	PreferFleetLaunch  *bool                    `json:"prefer_fleet_launch,omitempty" jsonschema:"when the devcontainer.json configures both a browser initialUrl and a fleetLaunch page, the built-in browser opens the Fleet Launch page (true) or initialUrl (false)"`
	LayoutPresets      []mcpLayoutPresetInput   `json:"layout_presets,omitempty" jsonschema:"session layout presets the TUI offers when a session is made, in order; replaces the list"`
	CustomMounts       []mcpMount               `json:"custom_mounts,omitempty" jsonschema:"folders every instance gets at path: the fleet's .mnt folder on the daemon host, or host_folder, an absolute daemon-host folder bound as it is; replaces the list (fleet_mount_set changes one)"`
	BuildkitServer     *bool                    `json:"buildkit_server,omitempty" jsonschema:"a shared BuildKit server, so the instances' docker builds share one build cache (devcontainer instances)"`
	DebCacheServer     *bool                    `json:"deb_cache_server,omitempty" jsonschema:"a shared apt package cache for the instances (HTTP apt sources)"`
	ImageCacheServer   *bool                    `json:"image_cache_server,omitempty" jsonschema:"a shared Docker Hub pull-through cache, for instances that run their own docker daemon"`
	CoderWorkspaceName *string                  `json:"coder_workspace_name,omitempty" jsonschema:"coder backend: names the fleet's workspaces <name>-<instance> instead of <fleet>-<instance> (alphanumerics and hyphens, at most 24)"`
	CoderTemplate      *string                  `json:"coder_template,omitempty" jsonschema:"coder backend: the template the fleet's workspaces are created from; empty lets coder pick, and also clears coder_preset and coder_parameters"`
	CoderPreset        *string                  `json:"coder_preset,omitempty" jsonschema:"coder backend: the template preset to create workspaces with; needs coder_template"`
	CoderParameters    []mcpCoderParameterInput `json:"coder_parameters,omitempty" jsonschema:"coder backend: template parameter values; needs coder_template; replaces the list"`
}

// mcpLayoutPresetInput is one layout preset to store.
type mcpLayoutPresetInput struct {
	Name         string   `json:"name" jsonschema:"preset name, unique within the fleet"`
	PaneCommands []string `json:"pane_commands" jsonschema:"one startup command per session pane, top then left; an empty string opens a plain shell. Its length is the pane count (1 to 32)"`
	Layout       *string  `json:"layout,omitempty" jsonschema:"tmux window_layout of the panes as fleet_settings reports it, whose first pane is the fleet TUI's; omit to keep the same-named preset's while its pane count is unchanged, empty for the default stacked split"`
}

// mcpCoderParameterInput is one Coder template parameter value.
type mcpCoderParameterInput struct {
	Name  string `json:"name" jsonschema:"template parameter name"`
	Value string `json:"value" jsonschema:"its value; may use ${GIT_URL}, ${GIT_BRANCH} and ${INSTANCE_NAME}"`
}

// mcpFleetSettings is every option of a fleet, as fleet_settings and
// fleet_edit report it.
type mcpFleetSettings struct {
	ClaudeCodeMount    bool                `json:"claude_code_mount"`
	CodexMount         bool                `json:"codex_mount"`
	AuggieMount        bool                `json:"auggie_mount"`
	FleetMCP           bool                `json:"fleet_mcp"`
	GhMount            bool                `json:"gh_mount"`
	HomeDir            string              `json:"home_dir,omitempty" jsonschema:"empty: the default /home/vscode"`
	PreferFleetLaunch  *bool               `json:"prefer_fleet_launch,omitempty" jsonschema:"absent until chosen: the TUI asks the first time both pages are configured"`
	LayoutPresets      []mcpLayoutPreset   `json:"layout_presets"`
	CustomMounts       []mcpMount          `json:"custom_mounts"`
	BuildkitServer     bool                `json:"buildkit_server"`
	DebCacheServer     bool                `json:"deb_cache_server"`
	ImageCacheServer   bool                `json:"image_cache_server"`
	CoderWorkspaceName string              `json:"coder_workspace_name,omitempty"`
	CoderTemplate      string              `json:"coder_template,omitempty"`
	CoderPreset        string              `json:"coder_preset,omitempty"`
	CoderParameters    []mcpCoderParameter `json:"coder_parameters"`
}

type mcpLayoutPreset struct {
	Name         string   `json:"name"`
	PaneCommands []string `json:"pane_commands"`
	Layout       string   `json:"layout,omitempty"`
}

// mcpCoderParameter is a stored parameter value, with what the TUI last
// fetched about it from the template.
type mcpCoderParameter struct {
	Name         string `json:"name"`
	Value        string `json:"value"`
	DefaultValue string `json:"default_value,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	Description  string `json:"description,omitempty"`
	Type         string `json:"type,omitempty"`
}

// FleetSettingsOutput is a fleet's options, returned by fleet_settings and
// fleet_edit.
type FleetSettingsOutput struct {
	Fleet    string           `json:"fleet"`
	Remote   string           `json:"remote,omitempty"`
	Settings mcpFleetSettings `json:"settings"`
	// Warnings are what the change was saved with but the caller should
	// know: a custom mount host folder missing on the daemon host.
	Warnings []string `json:"warnings,omitempty"`
}

func toMCPFleetSettings(s fleet.FleetSettings) mcpFleetSettings {
	out := mcpFleetSettings{
		ClaudeCodeMount:    s.ClaudeCodeMount,
		CodexMount:         s.CodexMount,
		AuggieMount:        s.AuggieMount,
		FleetMCP:           s.FleetMCP,
		GhMount:            s.GhMount,
		HomeDir:            s.HomeDir,
		PreferFleetLaunch:  s.PreferFleetLaunch,
		LayoutPresets:      []mcpLayoutPreset{},
		CustomMounts:       toMCPMounts(s, nil).Mounts,
		BuildkitServer:     s.BuildkitServer,
		DebCacheServer:     s.DebCacheServer,
		ImageCacheServer:   s.ImageCacheServer,
		CoderWorkspaceName: s.CoderWorkspaceName,
		CoderTemplate:      s.CoderTemplate,
		CoderPreset:        s.CoderPreset,
		CoderParameters:    []mcpCoderParameter{},
	}
	for _, p := range s.LayoutPresets {
		out.LayoutPresets = append(out.LayoutPresets, mcpLayoutPreset{Name: p.Name, PaneCommands: p.PaneCommands, Layout: p.Layout})
	}
	for _, p := range s.CoderParameters {
		out.CoderParameters = append(out.CoderParameters, mcpCoderParameter{
			Name:         p.Name,
			Value:        p.Value,
			DefaultValue: p.DefaultValue,
			DisplayName:  p.DisplayName,
			Description:  p.Description,
			Type:         p.Type,
		})
	}
	return out
}

// applyFleetSettings returns cur with in's fields set; SetFleetSettings (or
// normalizeFleetSettings, for a new fleet) checks the rest.
func applyFleetSettings(cur fleet.FleetSettings, in *mcpFleetSettingsInput) (fleet.FleetSettings, error) {
	setBool := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	setBool(&cur.ClaudeCodeMount, in.ClaudeCodeMount)
	setBool(&cur.CodexMount, in.CodexMount)
	setBool(&cur.AuggieMount, in.AuggieMount)
	setBool(&cur.FleetMCP, in.FleetMCP)
	setBool(&cur.GhMount, in.GhMount)
	setBool(&cur.BuildkitServer, in.BuildkitServer)
	setBool(&cur.DebCacheServer, in.DebCacheServer)
	setBool(&cur.ImageCacheServer, in.ImageCacheServer)
	if in.PreferFleetLaunch != nil {
		v := *in.PreferFleetLaunch
		cur.PreferFleetLaunch = &v
	}
	if in.HomeDir != nil {
		home := strings.TrimSpace(*in.HomeDir)
		if home != "" && !path.IsAbs(home) {
			return cur, fmt.Errorf("home_dir %q must be an absolute path in the container", home)
		}
		if home != "" {
			home = path.Clean(home)
		}
		cur.HomeDir = home
	}
	if in.LayoutPresets != nil {
		cur.LayoutPresets = layoutPresetsFrom(cur.LayoutPresets, in.LayoutPresets)
	}
	if in.CustomMounts != nil {
		next := cur
		next.CustomMounts, next.CustomMountHostFolders = nil, nil
		for _, m := range in.CustomMounts {
			var err error
			if next, err = fleet.SetCustomMount(next, m.Path, m.HostFolder); err != nil {
				return cur, err
			}
		}
		cur = next
	}
	if in.CoderWorkspaceName != nil {
		cur.CoderWorkspaceName = *in.CoderWorkspaceName
	}
	if in.CoderTemplate != nil {
		cur.CoderTemplate = strings.TrimSpace(*in.CoderTemplate)
		if cur.CoderTemplate == "" {
			// As the TUI does: no template, so nothing to preset or bind.
			cur.CoderPreset, cur.CoderParameters = "", nil
		}
	}
	if in.CoderPreset != nil {
		cur.CoderPreset = strings.TrimSpace(*in.CoderPreset)
	}
	if in.CoderParameters != nil {
		params, err := coderParametersFrom(cur.CoderParameters, in.CoderParameters)
		if err != nil {
			return cur, err
		}
		cur.CoderParameters = params
	}
	if cur.CoderTemplate == "" && (cur.CoderPreset != "" || len(cur.CoderParameters) > 0) &&
		(in.CoderPreset != nil || in.CoderParameters != nil) {
		return cur, errors.New("coder_preset and coder_parameters need a coder_template")
	}
	return cur, nil
}

// layoutPresetsFrom is the preset list in, an omitted layout taken from the
// same-named preset of cur while its pane count is unchanged (a layout of
// another pane count would not fit).
func layoutPresetsFrom(cur []fleet.LayoutPreset, in []mcpLayoutPresetInput) []fleet.LayoutPreset {
	out := []fleet.LayoutPreset{}
	for _, p := range in {
		preset := fleet.LayoutPreset{Name: p.Name, PaneCommands: p.PaneCommands}
		if p.Layout != nil {
			preset.Layout = *p.Layout
		} else if i := slices.IndexFunc(cur, func(c fleet.LayoutPreset) bool { return c.Name == strings.TrimSpace(p.Name) }); i >= 0 && cur[i].PaneCount() == len(p.PaneCommands) {
			preset.Layout = cur[i].Layout
		}
		out = append(out, preset)
	}
	return out
}

// coderParametersFrom is the parameter list in, each keeping what the TUI
// last fetched about it from the template.
func coderParametersFrom(cur []fleet.CoderParameter, in []mcpCoderParameterInput) ([]fleet.CoderParameter, error) {
	out := []fleet.CoderParameter{}
	for _, p := range in {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return nil, errors.New("a coder parameter has no name")
		}
		if slices.ContainsFunc(out, func(o fleet.CoderParameter) bool { return o.Name == name }) {
			return nil, fmt.Errorf("coder parameter %q is given twice", name)
		}
		param := fleet.CoderParameter{Name: name}
		if i := slices.IndexFunc(cur, func(c fleet.CoderParameter) bool { return c.Name == name }); i >= 0 {
			param = cur[i]
		}
		param.Value = strings.TrimSpace(p.Value)
		out = append(out, param)
	}
	return out, nil
}

// newFleetSettings is the settings fleet_up makes a fleet with: in over
// none, normalized as SetFleetSettings would, with its warnings.
func newFleetSettings(in *mcpFleetSettingsInput) (*fleet.FleetSettings, []string, error) {
	settings, err := applyFleetSettings(fleet.FleetSettings{}, in)
	if err != nil {
		return nil, nil, err
	}
	if settings, err = normalizeFleetSettings(settings); err != nil {
		return nil, nil, mcpErr(err)
	}
	return &settings, hostFolderWarnings(settings.CustomMountHostFolders), nil
}

// fleetSettingsOutput is the named fleet's options as persisted.
func (s *service) fleetSettingsOutput(ctx context.Context, fleetName string, warnings []string) (FleetSettingsOutput, error) {
	reply, err := s.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return FleetSettingsOutput{}, mcpErr(err)
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return FleetSettingsOutput{}, fmt.Errorf("fleet %q not found", fleetName)
	}
	return FleetSettingsOutput{
		Fleet:    fleetName,
		Remote:   pf.GetRemote(),
		Settings: toMCPFleetSettings(protoconv.FleetSettingsFromProto(pf.GetSettings())),
		Warnings: warnings,
	}, nil
}

type FleetSettingsInput struct {
	Fleet string `json:"fleet" jsonschema:"fleet name"`
}

func (s *service) mcpFleetSettings(ctx context.Context, _ *mcp.CallToolRequest, in FleetSettingsInput) (*mcp.CallToolResult, FleetSettingsOutput, error) {
	if in.Fleet == "" {
		return nil, FleetSettingsOutput{}, errors.New("fleet is required")
	}
	out, err := s.fleetSettingsOutput(ctx, in.Fleet, nil)
	return nil, out, err
}

type FleetEditInput struct {
	Fleet    string                 `json:"fleet" jsonschema:"fleet name"`
	Settings *mcpFleetSettingsInput `json:"settings" jsonschema:"the options to change; omit one to keep it"`
}

func (s *service) mcpFleetEdit(ctx context.Context, _ *mcp.CallToolRequest, in FleetEditInput) (*mcp.CallToolResult, FleetSettingsOutput, error) {
	if in.Fleet == "" || in.Settings == nil {
		return nil, FleetSettingsOutput{}, errors.New("fleet and settings are required")
	}
	_, warnings, err := s.mutateAutomation(ctx, in.Fleet, func(fs fleet.FleetSettings) (fleet.FleetSettings, error) {
		return applyFleetSettings(fs, in.Settings)
	})
	if err != nil {
		return nil, FleetSettingsOutput{}, err
	}
	out, err := s.fleetSettingsOutput(ctx, in.Fleet, warnings)
	return nil, out, err
}
