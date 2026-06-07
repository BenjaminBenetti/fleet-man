package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// config.go implements the server-owned config.json RPCs. config.json is folded
// into the same single-writer ownership as state.json (the muWrite lock) so it
// gets the same atomic-save guarantee and no torn-write/lost-update exposure.
//
// The config proto faithfully mirrors internal/state.Config field-for-field, so
// SetConfig round-trips the FULL config (browser tri-states + rich coder
// parameters included) without loss.

// GetConfig returns the current config (defaults when config.json is absent).
func (s *service) GetConfig(_ context.Context, _ *fleetgrpc.GetConfigRequest) (*fleetgrpc.GetConfigReply, error) {
	c, err := state.LoadConfig()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load config: %v", err)
	}
	return &fleetgrpc.GetConfigReply{Config: configToProto(c)}, nil
}

// SetConfig replaces the whole config (the settings page sends the full edited
// Config). It returns the post-save config so the caller picks up SaveConfig's
// applyDefaults() normalization (e.g. an unknown agent tool snapped to claude).
func (s *service) SetConfig(_ context.Context, req *fleetgrpc.SetConfigRequest) (*fleetgrpc.SetConfigReply, error) {
	s.muWrite.Lock()
	defer s.muWrite.Unlock()

	if err := state.SaveConfig(protoConfigToLegacy(req.GetConfig())); err != nil {
		return nil, status.Errorf(codes.Internal, "save config: %v", err)
	}
	saved, err := state.LoadConfig()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload config: %v", err)
	}
	return &fleetgrpc.SetConfigReply{Config: configToProto(saved)}, nil
}

// configToProto maps the legacy Config to the proto contract. `,omitempty`
// scalars are sent only when non-empty; tri-state *bool fields preserve their
// nil-vs-set presence.
func configToProto(c *state.Config) *fleetgrpc.Config {
	if c == nil {
		return &fleetgrpc.Config{}
	}
	pc := &fleetgrpc.Config{
		General:        &fleetgrpc.GeneralSettings{},
		Agent:          &fleetgrpc.AgentSettings{ToolSelection: string(c.AgentSettings.ToolSelection)},
		Dotfiles:       &fleetgrpc.DotfilesSettings{AutoInstall: c.DotfilesSettings.AutoInstall},
		Coder:          &fleetgrpc.CoderSettings{},
		Codespaces:     &fleetgrpc.CodespacesSettings{},
		Browser:        &fleetgrpc.BrowserSettings{},
		RemoteMcp: &fleetgrpc.RemoteMcpSettings{
			Enabled:    c.RemoteMcpSettings.Enabled,
			GatewayUrl: c.RemoteMcpSettings.GatewayURL,
		},
		DefaultBackend: backendToProto(fleet.BackendType(c.DefaultBackend)),
	}

	if c.GeneralSettings.TmuxVimKeys != nil {
		pc.General.TmuxVimKeys = boolptr(*c.GeneralSettings.TmuxVimKeys)
	}
	if c.GeneralSettings.ShowHelpText != nil {
		pc.General.ShowHelpText = boolptr(*c.GeneralSettings.ShowHelpText)
	}

	if c.DotfilesSettings.RepoURL != "" {
		pc.Dotfiles.Repo = strptr(c.DotfilesSettings.RepoURL)
	}
	if c.DotfilesSettings.InstallScript != "" {
		pc.Dotfiles.InstallScript = strptr(c.DotfilesSettings.InstallScript)
	}

	if c.CoderSettings.Template != "" {
		pc.Coder.Template = strptr(c.CoderSettings.Template)
	}
	if c.CoderSettings.Preset != "" {
		pc.Coder.Preset = strptr(c.CoderSettings.Preset)
	}
	for _, p := range c.CoderSettings.Parameters {
		pp := &fleetgrpc.CoderParameter{Name: p.Name, Value: p.Value}
		if p.DefaultValue != "" {
			pp.DefaultValue = strptr(p.DefaultValue)
		}
		if p.DisplayName != "" {
			pp.DisplayName = strptr(p.DisplayName)
		}
		if p.Description != "" {
			pp.Description = strptr(p.Description)
		}
		if p.Type != "" {
			pp.Type = strptr(p.Type)
		}
		pc.Coder.Parameters = append(pc.Coder.Parameters, pp)
	}

	if c.CodespacesSettings.Machine != "" {
		pc.Codespaces.Machine = strptr(c.CodespacesSettings.Machine)
	}
	if c.CodespacesSettings.IdleTimeout != "" {
		pc.Codespaces.IdleTimeout = strptr(c.CodespacesSettings.IdleTimeout)
	}
	if c.CodespacesSettings.DevcontainerPath != "" {
		pc.Codespaces.DevcontainerPath = strptr(c.CodespacesSettings.DevcontainerPath)
	}

	if c.BrowserSettings.MultipleBrowsersPerFleet != nil {
		pc.Browser.MultipleBrowsersPerFleet = boolptr(*c.BrowserSettings.MultipleBrowsersPerFleet)
	}
	if c.BrowserSettings.AutoSwitch != nil {
		pc.Browser.AutoSwitch = boolptr(*c.BrowserSettings.AutoSwitch)
	}

	return pc
}

// protoConfigToLegacy reconstructs the legacy Config from the proto. SaveConfig
// applies its own normalization, so we faithfully copy presence here and let it
// snap invalid values. Starting from a zero Config (not DefaultConfig) avoids
// masking a field the caller meant to clear.
func protoConfigToLegacy(pc *fleetgrpc.Config) *state.Config {
	c := &state.Config{}
	if pc == nil {
		return c
	}

	if g := pc.GetGeneral(); g != nil {
		if g.TmuxVimKeys != nil {
			v := g.GetTmuxVimKeys()
			c.GeneralSettings.TmuxVimKeys = &v
		}
		if g.ShowHelpText != nil {
			v := g.GetShowHelpText()
			c.GeneralSettings.ShowHelpText = &v
		}
	}

	if a := pc.GetAgent(); a != nil {
		c.AgentSettings.ToolSelection = state.AgentTool(a.GetToolSelection())
	}

	if d := pc.GetDotfiles(); d != nil {
		c.DotfilesSettings.AutoInstall = d.GetAutoInstall()
		c.DotfilesSettings.RepoURL = d.GetRepo()
		c.DotfilesSettings.InstallScript = d.GetInstallScript()
	}

	if cd := pc.GetCoder(); cd != nil {
		c.CoderSettings.Template = cd.GetTemplate()
		c.CoderSettings.Preset = cd.GetPreset()
		for _, p := range cd.GetParameters() {
			c.CoderSettings.Parameters = append(c.CoderSettings.Parameters, state.CoderParameter{
				Name:         p.GetName(),
				Value:        p.GetValue(),
				DefaultValue: p.GetDefaultValue(),
				DisplayName:  p.GetDisplayName(),
				Description:  p.GetDescription(),
				Type:         p.GetType(),
			})
		}
	}

	if cs := pc.GetCodespaces(); cs != nil {
		c.CodespacesSettings.Machine = cs.GetMachine()
		c.CodespacesSettings.IdleTimeout = cs.GetIdleTimeout()
		c.CodespacesSettings.DevcontainerPath = cs.GetDevcontainerPath()
	}

	if b := pc.GetBrowser(); b != nil {
		if b.MultipleBrowsersPerFleet != nil {
			v := b.GetMultipleBrowsersPerFleet()
			c.BrowserSettings.MultipleBrowsersPerFleet = &v
		}
		if b.AutoSwitch != nil {
			v := b.GetAutoSwitch()
			c.BrowserSettings.AutoSwitch = &v
		}
	}

	if rm := pc.GetRemoteMcp(); rm != nil {
		c.RemoteMcpSettings.Enabled = rm.GetEnabled()
		c.RemoteMcpSettings.GatewayURL = rm.GetGatewayUrl()
	}

	c.DefaultBackend = backendProtoToString(pc.GetDefaultBackend())
	return c
}

// backendProtoToString is the inverse of backendToProto for the config's
// default_backend (legacy stores a plain string, "" when unrecorded).
func backendProtoToString(b fleetgrpc.BackendType) string {
	switch b {
	case fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER:
		return string(fleet.BackendDevcontainer)
	case fleetgrpc.BackendType_BACKEND_TYPE_CODER:
		return string(fleet.BackendCoder)
	case fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES:
		return string(fleet.BackendCodespaces)
	default:
		return ""
	}
}
