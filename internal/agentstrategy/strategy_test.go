package agentstrategy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/admiralskill"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestOnlyClaudeSupportsTheFleetMCPSoFar(t *testing.T) {
	for _, tool := range tools {
		s := For(tool)
		if s.Tool() != tool {
			t.Errorf("For(%q).Tool() = %q", tool, s.Tool())
		}
		want := tool == state.AgentToolClaude
		if s.SupportsFleetMCP() != want {
			t.Errorf("%s: SupportsFleetMCP = %v, want %v", tool, s.SupportsFleetMCP(), want)
		}
		if _, ok := s.FleetMCP(FleetMCPParams{Dir: "/tmp/x", Bridge: []string{"fleet", "mcp-bridge"}}); ok != want {
			t.Errorf("%s: FleetMCP ok = %v, want %v", tool, ok, want)
		}
	}
}

func TestClaudeFleetMCPIsAnInlinePlugin(t *testing.T) {
	setup, ok := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{
		Dir:    "/tmp/fleet-mcp",
		Bridge: []string{"/usr/bin/fleet", "mcp-bridge"},
	})
	if !ok {
		t.Fatal("claude supports the fleet MCP")
	}
	files := map[string]File{}
	for _, f := range setup.Files {
		files[f.Path] = f
	}
	root := "/tmp/fleet-mcp/claude-plugin"

	var manifest struct{ Name, Version string }
	if err := json.Unmarshal(files[root+"/.claude-plugin/plugin.json"].Content, &manifest); err != nil || manifest.Name != "fleet" || manifest.Version == "" {
		t.Fatalf("plugin manifest = %+v (%v)", manifest, err)
	}
	var servers struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(files[root+"/.mcp.json"].Content, &servers); err != nil {
		t.Fatal(err)
	}
	fleetServer, ok := servers.MCPServers["fleet"]
	if !ok || fleetServer.Command != "/usr/bin/fleet" || len(fleetServer.Args) != 1 || fleetServer.Args[0] != "mcp-bridge" {
		t.Fatalf(".mcp.json servers = %+v", servers.MCPServers)
	}
	skill := files[root+"/skills/fleet-admiral/SKILL.md"]
	if !bytes.Equal(skill.Content, admiralskill.Content()) {
		t.Fatal("the plugin must carry the Fleet Admiral skill")
	}
	for path, f := range files {
		if f.Mode != 0o644 {
			t.Errorf("%s mode = %v, want 0644", path, f.Mode)
		}
	}
	if len(setup.Env) != 1 || setup.Env[0].Name != "CLAUDE_CODE_PLUGIN_DIRS" || setup.Env[0].Value != root || !setup.Env[0].PathList {
		t.Fatalf("env = %+v, want CLAUDE_CODE_PLUGIN_DIRS=%s prepended", setup.Env, root)
	}
}

func TestClaudeFleetMCPNeedsADirAndABridge(t *testing.T) {
	if _, ok := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{Bridge: []string{"fleet"}}); ok {
		t.Fatal("no dir: nothing to point the agent at")
	}
	if _, ok := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{Dir: "/tmp/x"}); ok {
		t.Fatal("no bridge: nothing to run")
	}
}

// TestExportsInAShell runs the rendered exports through sh: values survive
// quoting, a path list keeps what the user already had, and sourcing them
// again (a shell started from a shell) adds nothing.
func TestExportsInAShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	setup := Setup{Env: []EnvVar{
		{Name: "FLEET_TEST_PLAIN", Value: "it's $HOME"},
		{Name: "FLEET_TEST_LIST", Value: "/tmp/fleet mcp/plugin", PathList: true},
	}}
	exports := setup.Exports()
	script := exports + exports + `printf '%s|%s' "$FLEET_TEST_PLAIN" "$FLEET_TEST_LIST"`

	run := func(env ...string) string {
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := run("FLEET_TEST_LIST="); got != "it's $HOME|/tmp/fleet mcp/plugin" {
		t.Fatalf("unset list: %q", got)
	}
	if got := run("FLEET_TEST_LIST=/home/me/plugins"); got != "it's $HOME|/tmp/fleet mcp/plugin:/home/me/plugins" {
		t.Fatalf("existing list: %q", got)
	}
	if got := run("FLEET_TEST_LIST=/home/me/plugins:/tmp/fleet mcp/plugin"); got != "it's $HOME|/home/me/plugins:/tmp/fleet mcp/plugin" {
		t.Fatalf("already listed: %q", got)
	}
	if (Setup{}).Exports() != "" {
		t.Fatal("no env: no exports")
	}
}

// TestInstallFleetMCP: the install writes every supporting agent's files and
// returns exports that point at them; a second run rewrites nothing.
func TestInstallFleetMCP(t *testing.T) {
	dir := t.TempDir()
	exports, err := InstallFleetMCP(dir, []string{"/usr/bin/fleet", "mcp-bridge"})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "claude-plugin")
	if !strings.Contains(exports, "CLAUDE_CODE_PLUGIN_DIRS") || !strings.Contains(exports, root) {
		t.Fatalf("exports = %q, want CLAUDE_CODE_PLUGIN_DIRS pointing at %s", exports, root)
	}
	skillPath := filepath.Join(root, "skills", "fleet-admiral", "SKILL.md")
	skill, err := os.ReadFile(skillPath)
	if err != nil || !bytes.Equal(skill, admiralskill.Content()) {
		t.Fatalf("the admiral skill did not land intact: %v", err)
	}
	for _, name := range []string{".claude-plugin/plugin.json", ".mcp.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	// Unchanged content is left alone (every shell start runs this).
	before, _ := os.Stat(skillPath)
	again, err := InstallFleetMCP(dir, []string{"/usr/bin/fleet", "mcp-bridge"})
	if err != nil || again != exports {
		t.Fatalf("second install: %q, %v", again, err)
	}
	after, _ := os.Stat(skillPath)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged file was rewritten")
	}

	// Changed content (a fleet upgrade) is replaced.
	if err := os.WriteFile(skillPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFleetMCP(dir, []string{"/usr/bin/fleet", "mcp-bridge"}); err != nil {
		t.Fatal(err)
	}
	if skill, _ := os.ReadFile(skillPath); !bytes.Equal(skill, admiralskill.Content()) {
		t.Fatal("a stale file was not replaced")
	}
}

// TestClaudeLoadsThePlugin checks against a real Claude Code that it picks the
// plugin's MCP server up from CLAUDE_CODE_PLUGIN_DIRS alone. Opt-in
// (FLEET_TEST_CLAUDE=1): `claude mcp list` also health-checks every MCP server
// the user has configured.
func TestClaudeLoadsThePlugin(t *testing.T) {
	if os.Getenv("FLEET_TEST_CLAUDE") != "1" {
		t.Skip("set FLEET_TEST_CLAUDE=1 to check against the installed claude")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	dir := t.TempDir()
	exports, err := InstallFleetMCP(dir, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", exports+"exec "+claude+" mcp list")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "plugin:fleet:fleet") {
		t.Fatalf("claude did not load the plugin's MCP server:\n%s", out)
	}
}
