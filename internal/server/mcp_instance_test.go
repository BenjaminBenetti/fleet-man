package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func mcpSocket(fleetName, instanceName string) string {
	return filepath.Join(state.ControlDir(fleetName, instanceName), mcpbridge.SocketName)
}

// TestInstanceMCPSocketsFollowTheFleetSetting: every instance of a fleet with
// the Fleet MCP on gets a socket — whoever created it — and none of a fleet
// with it off; turning it off, or destroying the instance, removes the socket.
func TestInstanceMCPSocketsFollowTheFleetSetting(t *testing.T) {
	if !instanceSocketsSupported {
		t.Skip("instance sockets are Linux-only")
	}
	t.Setenv("HOME", shortTempDir(t))
	m := newTestInstanceMCP(t)

	provisionControlDir(t, "on", "mine", true)
	provisionControlDir(t, "on", "agent-1", true)
	provisionControlDir(t, "on", "unmounted", false) // e.g. coder: dir exists, not mounted
	provisionControlDir(t, "off", "mine", true)
	fleets := func(onSetting bool) *state.State {
		return &state.State{Fleets: map[string]*fleet.Fleet{
			"on": {Name: "on", Settings: fleet.FleetSettings{FleetMCP: onSetting}, Instances: []*fleet.Instance{
				{Name: "mine", Status: fleet.StatusRunning},
				{Name: "agent-1", Status: fleet.StatusStopped, Automated: true},
				{Name: "unmounted", Status: fleet.StatusRunning},
			}},
			"off": {Name: "off", Instances: []*fleet.Instance{{Name: "mine", Status: fleet.StatusRunning}}},
		}}
	}
	m.sync(fleets(true))

	for _, name := range []string{"mine", "agent-1"} {
		info, err := os.Stat(mcpSocket("on", name))
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
			t.Fatalf("socket for on/%s: %v %v, want a 0666 socket", name, info, err)
		}
	}
	if _, err := os.Stat(mcpSocket("on", "unmounted")); err == nil {
		t.Fatal("an instance that does not mount its control directory must not get a socket")
	}
	if _, err := os.Stat(mcpSocket("off", "mine")); err == nil {
		t.Fatal("a fleet with the Fleet MCP off must not get sockets")
	}

	// The setting is turned off: the sockets go.
	m.sync(fleets(false))
	if _, err := os.Stat(mcpSocket("on", "mine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be removed when the setting is turned off, stat err = %v", err)
	}

	// Back on, then the instance is destroyed: its socket goes with it.
	m.sync(fleets(true))
	if _, err := os.Stat(mcpSocket("on", "mine")); err != nil {
		t.Fatalf("socket should return with the setting: %v", err)
	}
	m.sync(&state.State{Fleets: map[string]*fleet.Fleet{}})
	if _, err := os.Stat(mcpSocket("on", "mine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be removed with its instance, stat err = %v", err)
	}
}

// TestInstanceMCPEnsureInstance: provisioning's hook opens a new instance's
// socket at once, but only for a fleet with the setting on.
func TestInstanceMCPEnsureInstance(t *testing.T) {
	if !instanceSocketsSupported {
		t.Skip("instance sockets are Linux-only")
	}
	t.Setenv("HOME", shortTempDir(t))
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"on":  {Name: "on", Settings: fleet.FleetSettings{FleetMCP: true}, Instances: []*fleet.Instance{{Name: "new", Status: fleet.StatusCreating}}},
		"off": {Name: "off", Instances: []*fleet.Instance{{Name: "new", Status: fleet.StatusCreating}}},
	}}); err != nil {
		t.Fatal(err)
	}
	m := newTestInstanceMCP(t)
	provisionControlDir(t, "on", "new", true)
	provisionControlDir(t, "off", "new", true)

	m.ensureInstance("on", "new")
	if !m.sockets.listening("on/new") {
		t.Fatal("a new instance of a fleet with the Fleet MCP on should get its socket at once")
	}
	m.ensureInstance("off", "new")
	m.ensureInstance("nofleet", "new")
	if m.sockets.listening("off/new") || m.sockets.listening("nofleet/new") {
		t.Fatal("no socket without the setting")
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
	provisionControlDir(t, "alpha", "dev-1", true)
	m.sync(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{FleetMCP: true}, Instances: []*fleet.Instance{{Name: "dev-1"}}},
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := net.Dial("unix", mcpSocket("alpha", "dev-1"))
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
	if !strings.Contains(instructions, `instance "dev-1" of fleet "alpha"`) || !strings.Contains(instructions, "fleet_down") {
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
	// The full tool set, by design: lifecycle, sessions and automation alike.
	for _, want := range []string{"fleet_up", "fleet_session_spawn", "fleet_agent_create", "fleet_destroy_fleet"} {
		if !names[want] {
			t.Fatalf("tool %q missing from the in-instance server", want)
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_version"})
	if err != nil || res.IsError {
		t.Fatalf("fleet_version: %v %+v", err, res)
	}
}
