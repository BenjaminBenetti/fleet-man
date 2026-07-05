package tui

import (
	"reflect"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestTriggersConverterRoundTrip locks in that the TUI's client-side trigger
// converters are lossless: the dialog persists by converting domain -> proto
// (triggersToProto) for SetFleetSettings, and renders by converting back
// (protoTriggersToLegacy), so any field dropped on the write would be silently
// lost on save. Covers a bash trigger's Script — the field whose omission here
// broke saving a bash trigger from the TUI.
func TestTriggersConverterRoundTrip(t *testing.T) {
	triggers := []fleet.Trigger{
		{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Prompt: "go", Cron: "0 0 * * *", Disabled: true},
		{Name: "hook", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: "push"},
		{Name: "poll", Type: fleet.TriggerBash, AgentNames: []string{"a"}, Prompt: "drain", Cron: "*/5 * * * *", Script: "test -s /var/queue"},
	}
	if got := protoTriggersToLegacy(triggersToProto(triggers)); !reflect.DeepEqual(got, triggers) {
		t.Fatalf("trigger round trip:\n got %+v\nwant %+v", got, triggers)
	}
}

// TestConfigToProtoEncodesFullConfig guards the client-side legacy->proto
// converter against drift from the server's reverse mapper: every field the
// settings page can edit — including the browser tri-states that the original
// proto could not represent — must be carried.
func TestConfigToProtoEncodesFullConfig(t *testing.T) {
	vim := false
	multi := true
	auto := false
	c := &state.Config{
		GeneralSettings:    state.GeneralSettings{TmuxVimKeys: &vim},
		AgentSettings:      state.AgentSettings{ToolSelection: state.AgentToolCodex},
		DotfilesSettings:   state.DotfilesSettings{AutoInstall: true, RepoURL: "git@x:d.git", InstallScript: "i.sh"},
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
	if pc.GetCodespaces().GetMachine() != "m1" || pc.GetCodespaces().GetIdleTimeout() != "30m" {
		t.Fatalf("codespaces: %v", pc.GetCodespaces())
	}
	if pc.GetDefaultBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODER {
		t.Fatalf("default_backend: %v", pc.GetDefaultBackend())
	}
}

// TestCoderFleetSettingsClientRoundTrip locks in that the TUI's client-side
// FleetSettings converters carry the per-fleet coder settings (issue #221) in
// BOTH directions: the edit-fleet dialog persists via fleetSettingsToProto
// (SetFleetSettings) and renders via protoFleetToLegacy, so a field dropped on
// either side is silently lost — exactly the client-converter gap that only a
// client round-trip test catches.
func TestCoderFleetSettingsClientRoundTrip(t *testing.T) {
	in := fleet.FleetSettings{
		CoderTemplate:      "tmpl",
		CoderPreset:        "preset-a",
		CoderWorkspaceName: "myws",
		CoderParameters: []fleet.CoderParameter{
			{Name: "p1", Value: "v1", DefaultValue: "d1", DisplayName: "P One", Description: "first", Type: "string"},
			{Name: "p2", Value: "${GIT_URL}"},
		},
	}
	back := protoFleetToLegacy(&fleetgrpc.Fleet{Name: "f", Settings: fleetSettingsToProto(in)})
	if !reflect.DeepEqual(back.Settings, in) {
		t.Fatalf("coder fleet-settings round trip:\n got %+v\nwant %+v", back.Settings, in)
	}
}

// TestRemoteMcpConfigRoundTripClient checks the remote-MCP settings survive both
// client converters, guarding against drift from the server's mappers.
func TestRemoteMcpConfigRoundTripClient(t *testing.T) {
	c := &state.Config{
		AgentSettings:     state.AgentSettings{ToolSelection: state.AgentToolClaude},
		RemoteMcpSettings: state.RemoteMcpSettings{Enabled: true, GatewayURL: "https://gateway.example.com", FleetEnabled: true},
	}

	pc := configToProto(c)
	if !pc.GetRemoteMcp().GetEnabled() || pc.GetRemoteMcp().GetGatewayUrl() != "https://gateway.example.com" {
		t.Fatalf("configToProto lost remote-mcp: %v", pc.GetRemoteMcp())
	}
	if !pc.GetRemoteMcp().GetFleetEnabled() {
		t.Fatalf("configToProto lost fleet_enabled: %v", pc.GetRemoteMcp())
	}

	back := protoConfigToLegacy(pc)
	if !back.RemoteMcpSettings.Enabled || back.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("protoConfigToLegacy lost remote-mcp: %+v", back.RemoteMcpSettings)
	}
	if !back.RemoteMcpSettings.FleetEnabled {
		t.Fatalf("protoConfigToLegacy lost fleet_enabled: %+v", back.RemoteMcpSettings)
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

// TestFleetSettingsCacheServersRoundTrip checks the deb/image cache bools survive
// both client converters (legacy->proto and proto->legacy).
func TestFleetSettingsCacheServersRoundTrip(t *testing.T) {
	// Off by default -> false on the wire.
	empty := fleetSettingsToProto(fleet.FleetSettings{})
	if empty.GetDebCacheServer() || empty.GetImageCacheServer() {
		t.Fatalf("unset cache servers should be false on the wire")
	}
	// Enabled survives legacy->proto.
	ps := fleetSettingsToProto(fleet.FleetSettings{DebCacheServer: true, ImageCacheServer: true})
	if !ps.GetDebCacheServer() || !ps.GetImageCacheServer() {
		t.Fatalf("cache servers lost in fleetSettingsToProto")
	}
	// And proto->legacy.
	back := protoFleetToLegacy(&fleetgrpc.Fleet{Name: "alpha", Settings: ps})
	if !back.Settings.DebCacheServer || !back.Settings.ImageCacheServer {
		t.Fatalf("cache servers lost in protoFleetToLegacy: %+v", back.Settings)
	}
}
