package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/admiralskill"
	"github.com/BenjaminBenetti/fleet-man/internal/agentstrategy"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_instance_test.go: the in-instance fleet MCP (issue #219). The sockets
// are served only where instance sockets are (Linux).

// provisionControlDir creates an instance's control directory and, when
// mounted, the marker provisioning drops when it bind-mounts it.
func provisionControlDir(t *testing.T, fleetName, instanceName string, mounted bool) string {
	t.Helper()
	dir := state.ControlDir(fleetName, instanceName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if mounted {
		if err := os.WriteFile(control.MountMarkerPath(dir), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newTestInstanceMCP(t *testing.T) *instanceMCP {
	t.Helper()
	svc := newService()
	t.Cleanup(svc.instanceMCP.close)
	return svc.instanceMCP
}

func TestInstanceMCPSocketsFollowState(t *testing.T) {
	if !instanceSocketsSupported {
		t.Skip("instance sockets are Linux-only")
	}
	t.Setenv("HOME", shortTempDir(t))
	m := newTestInstanceMCP(t)

	provisionControlDir(t, "f", "mcp", true)
	provisionControlDir(t, "f", "plain", true)      // agent without the fleet MCP
	provisionControlDir(t, "f", "unmounted", false) // e.g. coder: dir exists, not mounted
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"f": {Name: "f", Instances: []*fleet.Instance{
			{Name: "mcp", FleetMCP: true, Status: fleet.StatusRunning},
			{Name: "plain", Status: fleet.StatusRunning},
			{Name: "unmounted", FleetMCP: true, Status: fleet.StatusRunning},
		}},
	}}
	m.sync(st)

	sock := filepath.Join(state.ControlDir("f", "mcp"), mcpbridge.SocketName)
	info, err := os.Stat(sock)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
		t.Fatalf("socket for the fleet-MCP instance: %v %v, want a 0666 socket", info, err)
	}
	for _, name := range []string{"plain", "unmounted"} {
		if _, err := os.Stat(filepath.Join(state.ControlDir("f", name), mcpbridge.SocketName)); err == nil {
			t.Fatalf("instance %q must not get an MCP socket", name)
		}
	}

	// The instance is destroyed: its socket goes with it.
	m.sync(&state.State{Fleets: map[string]*fleet.Fleet{}})
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be removed with its instance, stat err = %v", err)
	}
}

// TestInstanceMCPServesTheFleetTools connects to an instance's socket the way
// `fleet mcp-bridge` does and runs a real MCP session over it.
func TestInstanceMCPServesTheFleetTools(t *testing.T) {
	if !instanceSocketsSupported {
		t.Skip("instance sockets are Linux-only")
	}
	t.Setenv("HOME", shortTempDir(t))
	m := newTestInstanceMCP(t)
	provisionControlDir(t, "alpha", "orchestrator-1", true)
	if err := m.ensure("alpha", "orchestrator-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := net.Dial("unix", filepath.Join(state.ControlDir("alpha", "orchestrator-1"), mcpbridge.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "t"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		t.Fatalf("MCP handshake over the instance socket: %v", err)
	}
	defer session.Close()

	instructions := session.InitializeResult().Instructions
	if !strings.Contains(instructions, `instance "orchestrator-1" of fleet "alpha"`) || !strings.Contains(instructions, "fleet_down") {
		t.Fatalf("the server should tell the agent where it runs and to clean up:\n%s", instructions)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"fleet_up", "fleet_session_spawn", "fleet_agent_create"} {
		if !names[want] {
			t.Fatalf("tool %q missing from the in-instance server", want)
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_version"})
	if err != nil || res.IsError {
		t.Fatalf("fleet_version: %v %+v", err, res)
	}
}

func TestInstanceMCPEnsureNeedsAMountedControlDir(t *testing.T) {
	if !instanceSocketsSupported {
		t.Skip("instance sockets are Linux-only")
	}
	t.Setenv("HOME", shortTempDir(t))
	m := newTestInstanceMCP(t)
	provisionControlDir(t, "f", "coder-1", false)
	err := m.ensure("f", "coder-1")
	if err == nil || !strings.Contains(err.Error(), "devcontainer") {
		t.Fatalf("ensure without a mounted control dir = %v, want a devcontainer hint", err)
	}
}

func TestFleetMCPExports(t *testing.T) {
	if !instanceSocketsSupported {
		t.Skip("instance sockets are Linux-only")
	}
	t.Setenv("HOME", shortTempDir(t))
	var written []agentstrategy.File
	orig := writeFleetMCPFiles
	writeFleetMCPFiles = func(_ backend.Backend, _ *fleet.Instance, files []agentstrategy.File) error {
		written = append(written, files...)
		return nil
	}
	t.Cleanup(func() { writeFleetMCPFiles = orig })

	svc := newService()
	t.Cleanup(svc.instanceMCP.close)
	provisionControlDir(t, "alpha", "orch", true)
	inst := &fleet.Instance{Name: "orch", ContainerID: "c1"}

	exports := svc.fleetMCPExports(&watchedAgent{fleet: "alpha", instance: "orch", command: fleet.DefaultAgentCommand}, inst, nil)
	if exports != "export CLAUDE_CODE_PLUGIN_DIRS='/tmp/fleet-mcp/claude-plugin'${CLAUDE_CODE_PLUGIN_DIRS:+:$CLAUDE_CODE_PLUGIN_DIRS}; " {
		t.Fatalf("exports = %q", exports)
	}
	var mcpJSON string
	for _, f := range written {
		if strings.HasSuffix(f.Path, "/.mcp.json") {
			mcpJSON = string(f.Content)
		}
	}
	if !strings.Contains(mcpJSON, `"/usr/bin/fleet"`) || !strings.Contains(mcpJSON, `"mcp-bridge"`) {
		t.Fatalf("the plugin must run the staged bridge; .mcp.json = %s", mcpJSON)
	}
	if !svc.instanceMCP.sockets.listening("alpha/orch") {
		t.Fatal("the launch must open the instance's socket before the agent starts")
	}

	// An agent fleet has no fleet MCP integration for launches without it.
	written = nil
	if got := svc.fleetMCPExports(&watchedAgent{fleet: "alpha", instance: "orch", command: "codex '${PROMPT}'"}, inst, nil); got != "" || written != nil {
		t.Fatalf("codex: exports %q, files %d — want neither", got, len(written))
	}

	// An instance that cannot reach the socket launches without it too.
	provisionControlDir(t, "alpha", "remote", false)
	if got := svc.fleetMCPExports(&watchedAgent{fleet: "alpha", instance: "remote", command: fleet.DefaultAgentCommand}, &fleet.Instance{Name: "remote"}, nil); got != "" {
		t.Fatalf("unmounted control dir: exports %q, want none", got)
	}

	// A failed write leaves the agent without it rather than pointing it at
	// files that are not there.
	writeFleetMCPFiles = func(backend.Backend, *fleet.Instance, []agentstrategy.File) error { return errors.New("boom") }
	if got := svc.fleetMCPExports(&watchedAgent{fleet: "alpha", instance: "orch", command: fleet.DefaultAgentCommand}, inst, nil); got != "" {
		t.Fatalf("failed write: exports %q, want none", got)
	}
}

// localScriptBackend runs RunScript's script with the local sh, standing in
// for `docker exec` into the instance.
type localScriptBackend struct {
	backend.Backend
	scripts int
}

func (l *localScriptBackend) RunScript(_, script string) (string, error) {
	l.scripts++
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return string(out), err
}

// TestWriteFleetMCPFiles runs the real write script: every file lands, with
// its content and mode, in one exec.
func TestWriteFleetMCPFiles(t *testing.T) {
	dir := t.TempDir()
	setup, ok := agentstrategy.For("claude").FleetMCP(agentstrategy.FleetMCPParams{Dir: dir, Bridge: []string{"/usr/bin/fleet", "mcp-bridge"}})
	if !ok {
		t.Fatal("claude supports the fleet MCP")
	}
	b := &localScriptBackend{}
	if err := writeFleetMCPFiles(b, &fleet.Instance{ContainerID: "c1"}, setup.Files); err != nil {
		t.Fatal(err)
	}
	if b.scripts != 1 {
		t.Fatalf("wrote with %d execs, want 1", b.scripts)
	}
	for _, f := range setup.Files {
		info, err := os.Stat(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != f.Mode {
			t.Errorf("%s mode = %v, want %v", f.Path, info.Mode().Perm(), f.Mode)
		}
		got, _ := os.ReadFile(f.Path)
		if !bytes.Equal(got, f.Content) {
			t.Errorf("%s content differs", f.Path)
		}
	}
	skill, _ := os.ReadFile(filepath.Join(dir, "claude-plugin", "skills", "fleet-admiral", "SKILL.md"))
	if !bytes.Equal(skill, admiralskill.Content()) {
		t.Fatal("the admiral skill did not land intact")
	}
}
