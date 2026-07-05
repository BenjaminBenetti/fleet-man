package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestSetConfigRoundTripsFullConfig is the regression guard for the config proto
// fix: the fields that the original proto could not represent — the browser
// tri-state toggles — must survive a SetConfig -> GetConfig round-trip without
// loss. (The rich coder parameters this test also used to guard moved to the
// per-fleet FleetSettings — issue #221 — and are guarded by the FleetSettings
// round-trip test in automation_convert_test.go.)
func TestSetConfigRoundTripsFullConfig(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	vimKeys := false
	multi := true
	autoSwitch := false
	in := &fleetgrpc.Config{
		General: &fleetgrpc.GeneralSettings{TmuxVimKeys: &vimKeys},
		Agent:   &fleetgrpc.AgentSettings{ToolSelection: "codex"},
		Dotfiles: &fleetgrpc.DotfilesSettings{
			AutoInstall:   true,
			Repo:          ptr("git@example.com:dotfiles.git"),
			InstallScript: ptr("install.sh"),
		},
		Codespaces: &fleetgrpc.CodespacesSettings{Machine: ptr("basicLinux32gb"), IdleTimeout: ptr("30m")},
		Browser:    &fleetgrpc.BrowserSettings{MultipleBrowsersPerFleet: &multi, AutoSwitch: &autoSwitch},

		DefaultBackend: fleetgrpc.BackendType_BACKEND_TYPE_CODER,
	}

	if _, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: in}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	reply, err := svc.GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	out := reply.GetConfig()

	if out.GetGeneral().GetTmuxVimKeys() {
		t.Fatalf("tmux_vim_keys should be false")
	}
	if out.GetAgent().GetToolSelection() != "codex" {
		t.Fatalf("agent tool mismatch: %q", out.GetAgent().GetToolSelection())
	}
	if !out.GetDotfiles().GetAutoInstall() || out.GetDotfiles().GetRepo() != "git@example.com:dotfiles.git" || out.GetDotfiles().GetInstallScript() != "install.sh" {
		t.Fatalf("dotfiles mismatch: %v", out.GetDotfiles())
	}
	// Browser tri-states — the headline regression.
	if !out.GetBrowser().GetMultipleBrowsersPerFleet() {
		t.Fatalf("multiple_browsers_per_fleet lost")
	}
	if out.GetBrowser().AutoSwitch == nil || out.GetBrowser().GetAutoSwitch() {
		t.Fatalf("auto_switch tri-state lost (want explicit false): %v", out.GetBrowser().AutoSwitch)
	}
	if out.GetCodespaces().GetMachine() != "basicLinux32gb" || out.GetCodespaces().GetIdleTimeout() != "30m" {
		t.Fatalf("codespaces mismatch: %v", out.GetCodespaces())
	}
	if out.GetDefaultBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODER {
		t.Fatalf("default_backend mismatch: %v", out.GetDefaultBackend())
	}

	// And the bytes on disk must be exactly what the legacy SaveConfig writes,
	// so a client still reading config.json directly stays consistent.
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.BrowserSettings.MultipleBrowsersPerFleet == nil || !*loaded.BrowserSettings.MultipleBrowsersPerFleet {
		t.Fatalf("disk config lost multiple_browsers_per_fleet")
	}
}

// TestSetConfigRoundTripsRemoteMcp guards the remote-MCP settings: enabled and
// gateway_url must survive a SetConfig -> GetConfig round-trip and land on disk.
// (The computed Public MCP URL is deliberately NOT a Config field — it travels
// over the Watch RemoteMcpStatus event — so there is nothing to round-trip for
// it here.)
func TestSetConfigRoundTripsRemoteMcp(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	in := &fleetgrpc.Config{
		Agent:     &fleetgrpc.AgentSettings{ToolSelection: "claude"},
		RemoteMcp: &fleetgrpc.RemoteMcpSettings{Enabled: true, GatewayUrl: "https://gateway.example.com", FleetEnabled: true},
	}
	if _, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: in}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	reply, err := svc.GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	rm := reply.GetConfig().GetRemoteMcp()
	if !rm.GetEnabled() {
		t.Fatalf("enabled lost on round-trip")
	}
	if rm.GetGatewayUrl() != "https://gateway.example.com" {
		t.Fatalf("gateway_url mismatch: %q", rm.GetGatewayUrl())
	}
	if !rm.GetFleetEnabled() {
		t.Fatalf("fleet_enabled lost on round-trip")
	}

	// And the bytes on disk match what the legacy SaveConfig writes.
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.RemoteMcpSettings.Enabled || loaded.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("disk config lost remote-mcp settings: %+v", loaded.RemoteMcpSettings)
	}
	if !loaded.RemoteMcpSettings.FleetEnabled {
		t.Fatalf("disk config lost fleet_enabled: %+v", loaded.RemoteMcpSettings)
	}
}
