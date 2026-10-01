package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPEnv: `fleet mcp-env` writes the agents' fleet MCP setup under the
// user's cache directory and prints exports pointing at it; the plugin's MCP
// server is this same binary's mcp-bridge.
func TestMCPEnv(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", t.TempDir())

	out, err := runCLI(t, "mcp-env")
	if err != nil {
		t.Fatalf("mcp-env: %v", err)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(cacheDir, "fleet", "mcp", "claude-plugin")
	if !strings.Contains(out, "CLAUDE_CODE_PLUGIN_DIRS") || !strings.Contains(out, plugin) {
		t.Fatalf("exports = %q, want CLAUDE_CODE_PLUGIN_DIRS pointing at %s", out, plugin)
	}
	if _, err := os.Stat(filepath.Join(plugin, "skills", "fleet-admiral", "SKILL.md")); err != nil {
		t.Fatalf("the plugin's skill was not written: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(plugin, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var servers struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &servers); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if s := servers.MCPServers["fleet"]; s.Command != self || len(s.Args) != 1 || s.Args[0] != "mcp-bridge" {
		t.Fatalf("the plugin should run this binary's mcp-bridge, got %+v", servers.MCPServers)
	}
}
