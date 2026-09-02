package protoconv

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// ConfigToProto maps the legacy Config to the proto contract. `,omitempty`
// scalars are sent only when non-empty; tri-state *bool fields preserve their
// nil-vs-set presence.
func ConfigToProto(c *configutil.Config) *fleetgrpc.Config {
	if c == nil {
		return &fleetgrpc.Config{}
	}
	pc := &fleetgrpc.Config{
		General:    &fleetgrpc.GeneralSettings{},
		Agent:      &fleetgrpc.AgentSettings{ToolSelection: string(c.AgentSettings.ToolSelection)},
		Dotfiles:   &fleetgrpc.DotfilesSettings{AutoInstall: c.DotfilesSettings.AutoInstall},
		Codespaces: &fleetgrpc.CodespacesSettings{},
		Browser:    &fleetgrpc.BrowserSettings{},
		RemoteMcp: &fleetgrpc.RemoteMcpSettings{
			Enabled:        c.RemoteMcpSettings.Enabled,
			GatewayUrl:     c.RemoteMcpSettings.GatewayURL,
			FleetEnabled:   c.RemoteMcpSettings.FleetEnabled,
			WebhookEnabled: c.RemoteMcpSettings.WebhookEnabled,
			FleetMode:      c.RemoteMcpSettings.FleetMode,
		},
		DefaultBackend: BackendToProto(fleet.BackendType(c.DefaultBackend)),
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

// ConfigFromProto reconstructs the legacy Config from the proto, writing onto
// base and returning it.
//
// The base parameter is the one deliberate behavioral fork between the two
// callers this converter was consolidated from:
//   - The server's SetConfig (write path) passes a ZERO &configutil.Config{} so a
//     field the caller meant to clear is not masked by a default; SaveConfig's
//     applyDefaults() then normalizes (e.g. an empty agent tool snaps to
//     claude).
//   - The TUI's read path passes configutil.DefaultConfig() so absent optional
//     fields render as their defaults.
//
// ToolSelection is only overwritten when non-empty: with a zero base that is
// indistinguishable from a faithful copy (empty stays empty), and with a
// default base it keeps the default from being clobbered by an unset field.
// A nil base gets a zero Config.
func ConfigFromProto(pc *fleetgrpc.Config, base *configutil.Config) *configutil.Config {
	c := base
	if c == nil {
		c = &configutil.Config{}
	}
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

	if a := pc.GetAgent(); a != nil && a.GetToolSelection() != "" {
		c.AgentSettings.ToolSelection = configutil.AgentTool(a.GetToolSelection())
	}

	if d := pc.GetDotfiles(); d != nil {
		c.DotfilesSettings.AutoInstall = d.GetAutoInstall()
		c.DotfilesSettings.RepoURL = d.GetRepo()
		c.DotfilesSettings.InstallScript = d.GetInstallScript()
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
		c.RemoteMcpSettings.FleetEnabled = rm.GetFleetEnabled()
		c.RemoteMcpSettings.WebhookEnabled = rm.GetWebhookEnabled()
		c.RemoteMcpSettings.FleetMode = rm.GetFleetMode()
	}

	c.DefaultBackend = string(BackendFromProto(pc.GetDefaultBackend()))
	return c
}
