package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds user preferences.
type Config struct {
	GeneralSettings    GeneralSettings    `json:"general_settings"`
	AgentSettings      AgentSettings      `json:"agent_settings"`
	DotfilesSettings   DotfilesSettings   `json:"dotfiles_settings"`
	CoderSettings      CoderSettings      `json:"coder_settings"`
	CodespacesSettings CodespacesSettings `json:"codespaces_settings"`
	BrowserSettings    BrowserSettings    `json:"browser_settings"`
	DefaultBackend     string             `json:"default_backend,omitempty"` // "devcontainer", "coder", or "codespaces"
}

// DefaultConfig returns a config with default values applied.
func DefaultConfig() *Config {
	return &Config{
		AgentSettings: AgentSettings{
			ToolSelection: AgentToolClaude,
		},
	}
}

// applyDefaults normalises the config in place: invalid agent tools fall
// back to the default, and string fields are trimmed.
func (c *Config) applyDefaults() {
	if c == nil {
		return
	}

	if _, ok := validAgentTools[c.AgentSettings.ToolSelection]; !ok {
		c.AgentSettings.ToolSelection = AgentToolClaude
	}

	c.DotfilesSettings.RepoURL = strings.TrimSpace(c.DotfilesSettings.RepoURL)
	c.DotfilesSettings.InstallScript = strings.TrimSpace(c.DotfilesSettings.InstallScript)

	c.CoderSettings.Template = strings.TrimSpace(c.CoderSettings.Template)
	c.CoderSettings.Preset = strings.TrimSpace(c.CoderSettings.Preset)
}

// ConfigPath returns the path to the config file.
func ConfigPath() string {
	return filepath.Join(FleetDir(), "config.json")
}

// LoadConfig reads the config from disk. Returns defaults if the file doesn't exist.
func LoadConfig() (*Config, error) {
	c := DefaultConfig()

	data, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	c.applyDefaults()
	return c, nil
}

// SaveConfig writes the config to disk.
func SaveConfig(c *Config) error {
	if c == nil {
		c = DefaultConfig()
	}
	c.applyDefaults()

	if err := os.MkdirAll(FleetDir(), 0755); err != nil {
		return fmt.Errorf("creating fleet dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(ConfigPath(), data, 0644); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	return nil
}
