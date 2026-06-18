package state

import (
	"encoding/json"
	"os"
	"testing"
)

func TestLoadConfigReturnsDefaultsWhenMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if config.AgentSettings.ToolSelection != AgentToolClaude {
		t.Fatalf("ToolSelection = %q, want %q", config.AgentSettings.ToolSelection, AgentToolClaude)
	}
}

func TestLoadConfigAppliesDefaultsToEmptyJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := os.MkdirAll(FleetDir(), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(ConfigPath(), []byte("{}"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if config.AgentSettings.ToolSelection != AgentToolClaude {
		t.Fatalf("ToolSelection = %q, want %q", config.AgentSettings.ToolSelection, AgentToolClaude)
	}
}

func TestLoadConfigPreservesValidToolSelections(t *testing.T) {
	tests := []struct {
		name string
		tool AgentTool
	}{
		{name: "codex", tool: AgentToolCodex},
		{name: "claude", tool: AgentToolClaude},
		{name: "gemini", tool: AgentToolGemini},
		{name: "copilot", tool: AgentToolCopilot},
		{name: "auggie", tool: AgentToolAuggie},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			if err := os.MkdirAll(FleetDir(), 0755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}

			payload := []byte(`{"agent_settings":{"tool_selection":"` + string(tt.tool) + `"}}`)
			if err := os.WriteFile(ConfigPath(), payload, 0644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			config, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}

			if config.AgentSettings.ToolSelection != tt.tool {
				t.Fatalf("ToolSelection = %q, want %q", config.AgentSettings.ToolSelection, tt.tool)
			}
		})
	}
}

func TestLoadConfigDefaultsInvalidToolSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := os.MkdirAll(FleetDir(), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(`{"agent_settings":{"tool_selection":"invalid"}}`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if config.AgentSettings.ToolSelection != AgentToolClaude {
		t.Fatalf("ToolSelection = %q, want %q", config.AgentSettings.ToolSelection, AgentToolClaude)
	}
}

func TestSaveConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := &Config{
		AgentSettings: AgentSettings{
			ToolSelection: AgentToolCopilot,
		},
	}

	if err := SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	agentSettings, ok := raw["agent_settings"].(map[string]any)
	if !ok {
		t.Fatalf("agent_settings missing or wrong type: %#v", raw["agent_settings"])
	}
	if got := agentSettings["tool_selection"]; got != string(AgentToolCopilot) {
		t.Fatalf("tool_selection = %#v, want %q", got, AgentToolCopilot)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if got.AgentSettings.ToolSelection != AgentToolCopilot {
		t.Fatalf("ToolSelection = %q, want %q", got.AgentSettings.ToolSelection, AgentToolCopilot)
	}
}

func TestLoadConfigDefaultDotfilesSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if config.DotfilesSettings.RepoURL != "" {
		t.Fatalf("RepoURL = %q, want empty", config.DotfilesSettings.RepoURL)
	}
	if config.DotfilesSettings.InstallScript != "" {
		t.Fatalf("InstallScript = %q, want empty", config.DotfilesSettings.InstallScript)
	}
}

func TestSaveConfigRoundTripDotfiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := &Config{
		AgentSettings: AgentSettings{
			ToolSelection: AgentToolClaude,
		},
		DotfilesSettings: DotfilesSettings{
			RepoURL:       "https://github.com/user/dotfiles",
			InstallScript: "install.sh",
		},
	}

	if err := SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	dotfiles, ok := raw["dotfiles_settings"].(map[string]any)
	if !ok {
		t.Fatalf("dotfiles_settings missing or wrong type: %#v", raw["dotfiles_settings"])
	}
	if got := dotfiles["repo_url"]; got != "https://github.com/user/dotfiles" {
		t.Fatalf("repo_url = %#v, want %q", got, "https://github.com/user/dotfiles")
	}
	if got := dotfiles["install_script"]; got != "install.sh" {
		t.Fatalf("install_script = %#v, want %q", got, "install.sh")
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if got.DotfilesSettings.RepoURL != "https://github.com/user/dotfiles" {
		t.Fatalf("RepoURL = %q, want %q", got.DotfilesSettings.RepoURL, "https://github.com/user/dotfiles")
	}
	if got.DotfilesSettings.InstallScript != "install.sh" {
		t.Fatalf("InstallScript = %q, want %q", got.DotfilesSettings.InstallScript, "install.sh")
	}
}

func TestSaveConfigRoundTripAutoInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := &Config{
		AgentSettings: AgentSettings{
			ToolSelection: AgentToolClaude,
		},
		DotfilesSettings: DotfilesSettings{
			RepoURL:       "https://github.com/user/dotfiles",
			InstallScript: "install.sh",
			AutoInstall:   true,
		},
	}

	if err := SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if !got.DotfilesSettings.AutoInstall {
		t.Fatal("AutoInstall = false, want true")
	}
}

func TestSaveConfigRoundTripRemoteMcp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := &Config{
		AgentSettings:     AgentSettings{ToolSelection: AgentToolClaude},
		RemoteMcpSettings: RemoteMcpSettings{Enabled: true, GatewayURL: "https://gateway.example.com"},
	}

	if err := SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if !got.RemoteMcpSettings.Enabled {
		t.Fatal("RemoteMcpSettings.Enabled = false, want true")
	}
	if got.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("GatewayURL = %q, want %q", got.RemoteMcpSettings.GatewayURL, "https://gateway.example.com")
	}
}

func TestApplyDefaultsTrimsRemoteMcpGatewayURL(t *testing.T) {
	c := &Config{RemoteMcpSettings: RemoteMcpSettings{GatewayURL: "  https://gateway.example.com\n"}}
	c.applyDefaults()
	if c.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("GatewayURL not trimmed: %q", c.RemoteMcpSettings.GatewayURL)
	}
}

func TestTmuxVimKeysDefaultsToTrue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if !config.GeneralSettings.TmuxVimKeysEnabled() {
		t.Fatal("TmuxVimKeysEnabled() = false, want true (default)")
	}
}

func TestSaveConfigRoundTripTmuxVimKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	off := false
	want := &Config{
		AgentSettings: AgentSettings{
			ToolSelection: AgentToolClaude,
		},
		GeneralSettings: GeneralSettings{
			TmuxVimKeys: &off,
		},
	}

	if err := SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if got.GeneralSettings.TmuxVimKeysEnabled() {
		t.Fatal("TmuxVimKeysEnabled() = true, want false")
	}
}

func TestApplyDefaultsTrimsDotfilesWhitespace(t *testing.T) {
	config := &Config{
		DotfilesSettings: DotfilesSettings{
			RepoURL:       "  https://github.com/user/dotfiles  ",
			InstallScript: "  install.sh\n",
		},
	}

	config.applyDefaults()

	if config.DotfilesSettings.RepoURL != "https://github.com/user/dotfiles" {
		t.Fatalf("RepoURL = %q, want trimmed", config.DotfilesSettings.RepoURL)
	}
	if config.DotfilesSettings.InstallScript != "install.sh" {
		t.Fatalf("InstallScript = %q, want trimmed", config.DotfilesSettings.InstallScript)
	}
}
