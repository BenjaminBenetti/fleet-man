package tui

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestConfigToProtoEncodesFullConfig guards the client-side legacy->proto
// converter against drift from the server's reverse mapper: every field the
// settings page can edit — including the browser tri-states and the rich coder
// parameters that the original proto could not represent — must be carried.
func TestConfigToProtoEncodesFullConfig(t *testing.T) {
	vim := false
	multi := true
	auto := false
	c := &state.Config{
		GeneralSettings:  state.GeneralSettings{TmuxVimKeys: &vim},
		AgentSettings:    state.AgentSettings{ToolSelection: state.AgentToolCodex},
		DotfilesSettings: state.DotfilesSettings{AutoInstall: true, RepoURL: "git@x:d.git", InstallScript: "i.sh"},
		CoderSettings: state.CoderSettings{
			Template: "tmpl", Preset: "p",
			Parameters: []state.CoderParameter{
				{Name: "n1", Value: "v1", DefaultValue: "d1", DisplayName: "N1", Description: "desc", Type: "string"},
			},
		},
		CodespacesSettings: state.CodespacesSettings{Machine: "m1", IdleTimeout: "30m"},
		BrowserSettings:    state.BrowserSettings{MultipleBrowsersPerFleet: &multi, AutoSwitch: &auto},
		DefaultBackend:     string(fleet.BackendCoder),
	}

	pc := configToProto(c)

	if pc.GetGeneral().GetTmuxVimKeys() {
		t.Fatal("tmux_vim_keys should encode false")
	}
	if pc.GetAgent().GetToolSelection() != "codex" {
		t.Fatalf("agent tool: %q", pc.GetAgent().GetToolSelection())
	}
	if !pc.GetDotfiles().GetAutoInstall() || pc.GetDotfiles().GetRepo() != "git@x:d.git" || pc.GetDotfiles().GetInstallScript() != "i.sh" {
		t.Fatalf("dotfiles: %v", pc.GetDotfiles())
	}
	if !pc.GetBrowser().GetMultipleBrowsersPerFleet() {
		t.Fatal("multiple_browsers_per_fleet lost")
	}
	if pc.GetBrowser().AutoSwitch == nil || pc.GetBrowser().GetAutoSwitch() {
		t.Fatalf("auto_switch tri-state lost (want explicit false): %v", pc.GetBrowser().AutoSwitch)
	}
	ps := pc.GetCoder().GetParameters()
	if len(ps) != 1 || ps[0].GetName() != "n1" || ps[0].GetDefaultValue() != "d1" ||
		ps[0].GetDisplayName() != "N1" || ps[0].GetDescription() != "desc" || ps[0].GetType() != "string" {
		t.Fatalf("rich coder param lost: %v", ps)
	}
	if pc.GetCodespaces().GetMachine() != "m1" || pc.GetCodespaces().GetIdleTimeout() != "30m" {
		t.Fatalf("codespaces: %v", pc.GetCodespaces())
	}
	if pc.GetDefaultBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODER {
		t.Fatalf("default_backend: %v", pc.GetDefaultBackend())
	}
}

// TestRemoteMcpConfigRoundTripClient checks the remote-MCP settings survive both
// client converters, guarding against drift from the server's mappers.
func TestRemoteMcpConfigRoundTripClient(t *testing.T) {
	c := &state.Config{
		AgentSettings:     state.AgentSettings{ToolSelection: state.AgentToolClaude},
		RemoteMcpSettings: state.RemoteMcpSettings{Enabled: true, GatewayURL: "https://gateway.example.com"},
	}

	pc := configToProto(c)
	if !pc.GetRemoteMcp().GetEnabled() || pc.GetRemoteMcp().GetGatewayUrl() != "https://gateway.example.com" {
		t.Fatalf("configToProto lost remote-mcp: %v", pc.GetRemoteMcp())
	}

	back := protoConfigToLegacy(pc)
	if !back.RemoteMcpSettings.Enabled || back.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("protoConfigToLegacy lost remote-mcp: %+v", back.RemoteMcpSettings)
	}
}

// TestFleetSettingsToProtoPreservesTriState checks the PreferFleetLaunch
// nil-vs-set distinction survives the client converter.
func TestFleetSettingsToProtoPreservesTriState(t *testing.T) {
	// Unset -> nil on the wire.
	if got := fleetSettingsToProto(fleet.FleetSettings{}); got.PreferFleetLaunch != nil {
		t.Fatalf("unset PreferFleetLaunch should be nil, got %v", *got.PreferFleetLaunch)
	}
	// Explicit false -> present and false.
	f := false
	got := fleetSettingsToProto(fleet.FleetSettings{PreferFleetLaunch: &f, HomeDir: "/home/x"})
	if got.PreferFleetLaunch == nil || got.GetPreferFleetLaunch() {
		t.Fatalf("explicit-false PreferFleetLaunch lost: %v", got.PreferFleetLaunch)
	}
	if got.GetHomeDir() != "/home/x" {
		t.Fatalf("home dir: %q", got.GetHomeDir())
	}
}

// TestFleetSettingsBuildkitServerRoundTrip checks the BuildkitServer bool
// survives both client converters (legacy->proto and proto->legacy).
func TestFleetSettingsBuildkitServerRoundTrip(t *testing.T) {
	// Off by default -> false on the wire.
	if fleetSettingsToProto(fleet.FleetSettings{}).GetBuildkitServer() {
		t.Fatalf("unset BuildkitServer should be false on the wire")
	}
	// Enabled survives legacy->proto.
	ps := fleetSettingsToProto(fleet.FleetSettings{BuildkitServer: true})
	if !ps.GetBuildkitServer() {
		t.Fatalf("BuildkitServer lost in fleetSettingsToProto")
	}
	// And proto->legacy.
	back := protoFleetToLegacy(&fleetgrpc.Fleet{Name: "alpha", Settings: ps})
	if !back.Settings.BuildkitServer {
		t.Fatalf("BuildkitServer lost in protoFleetToLegacy: %+v", back.Settings)
	}
}
