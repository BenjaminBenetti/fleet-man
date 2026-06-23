package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpConnect builds the fleet MCP server for svc and returns an in-memory client
// session wired to it (no HTTP/socket). It also asserts newMCPServer doesn't
// panic — which catches AddTool schema-inference failures (e.g. a malformed
// jsonschema struct tag) that only surface at registration time.
func mcpConnect(t *testing.T, svc *service) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := newMCPServer(svc)
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callJSON calls a tool, fails on a tool error, and unmarshals its text content
// into v.
func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, v any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: unexpected tool error: %s", name, toolText(res))
	}
	if v == nil {
		return
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s: no content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s: first content is %T, want *mcp.TextContent", name, res.Content[0])
	}
	if err := json.Unmarshal([]byte(tc.Text), v); err != nil {
		t.Fatalf("%s: unmarshal %q: %v", name, tc.Text, err)
	}
}

func toolText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// TestListenMCPSkipsOccupiedPort verifies the port search increments past a
// bound port — the core of the issue's "increment until a free port is found".
func TestListenMCPSkipsOccupiedPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy: %v", err)
	}
	defer occupied.Close()
	base := occupied.Addr().(*net.TCPAddr).Port

	lis, port, err := listenMCP(base)
	if err != nil {
		t.Fatalf("listenMCP(%d): %v", base, err)
	}
	defer lis.Close()
	if port <= base {
		t.Fatalf("expected a port above the occupied %d, got %d", base, port)
	}
	if got := lis.Addr().(*net.TCPAddr).Port; got != port {
		t.Fatalf("returned port %d != listener port %d", port, got)
	}
}

// TestNewMCPServerRegistersAllTools asserts every fleet tool registers (no
// AddTool panic) and is advertised over tools/list.
func TestNewMCPServerRegistersAllTools(t *testing.T) {
	isolateFleetDir(t)
	cs := mcpConnect(t, newService())

	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	want := []string{
		"fleet_list", "fleet_status", "fleet_version", "fleet_logs", "fleet_restore_backup",
		"fleet_up", "fleet_start", "fleet_stop", "fleet_down",
		"fleet_destroy_fleet", "fleet_clone", "fleet_rebuild", "fleet_job_status",
		"fleet_exec", "fleet_session_spawn", "fleet_session_exec", "fleet_session_read",
		"fleet_session_list",
		"fleet_automation_list", "fleet_agent_create", "fleet_agent_update", "fleet_agent_delete",
		"fleet_trigger_create", "fleet_trigger_update", "fleet_trigger_delete",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q not registered", name)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("registered %d tools, want %d: %v", len(res.Tools), len(want), got)
	}
}

// TestMCPReadToolsReflectState exercises the read tools against seeded state.
func TestMCPReadToolsReflectState(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@example.com:a.git", Instances: []*fleet.Instance{
			{Name: "i1", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "cid1", Branch: "main"},
			{Name: "i2", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cs := mcpConnect(t, newService())

	var list FleetListOutput
	callJSON(t, cs, "fleet_list", map[string]any{}, &list)
	if len(list.Instances) != 2 {
		t.Fatalf("fleet_list: got %d instances, want 2: %+v", len(list.Instances), list.Instances)
	}

	// The fleet filter narrows to the named fleet (and a miss yields nothing).
	var filtered FleetListOutput
	callJSON(t, cs, "fleet_list", map[string]any{"fleet": "ghost"}, &filtered)
	if len(filtered.Instances) != 0 {
		t.Fatalf("fleet_list ghost: want 0, got %d", len(filtered.Instances))
	}

	var st FleetStatusOutput
	callJSON(t, cs, "fleet_status", map[string]any{}, &st)
	if st.TotalFleets != 1 || st.TotalInstances != 2 || st.Running != 1 || st.Stopped != 1 {
		t.Fatalf("fleet_status mismatch: %+v", st)
	}

	var ver FleetVersionOutput
	callJSON(t, cs, "fleet_version", map[string]any{}, &ver)
	if ver.Pid == 0 {
		t.Fatalf("fleet_version: pid not reported: %+v", ver)
	}
}

// TestMCPToolErrors confirms a missing instance and missing required input both
// surface as MCP tool errors (IsError), not transport errors.
func TestMCPToolErrors(t *testing.T) {
	isolateFleetDir(t)
	cs := mcpConnect(t, newService())

	// Unknown instance -> tool error from the resolve.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "fleet_exec", Arguments: map[string]any{"fleet": "ghost", "instance": "x", "command": []string{"echo", "hi"}},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "not found") {
		t.Fatalf("want NotFound tool error, got IsError=%v text=%q", res.IsError, toolText(res))
	}

	// Missing required field -> the SDK rejects via input-schema validation.
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "fleet_up", Arguments: map[string]any{"fleet": "x"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want input-validation tool error for missing instance, got %q", toolText(res))
	}
}

// TestParseMCPSessions covers the tmux list-sessions parser: field mapping,
// the attached flag, epoch->RFC3339 rendering, colon-bearing names (name is the
// trailing field), and skipping of blank/short/garbage lines.
func TestParseMCPSessions(t *testing.T) {
	const created = 1700000000
	wantCreated := time.Unix(created, 0).UTC().Format(time.RFC3339)
	output := strings.Join([]string{
		"3:1:" + strconv.Itoa(created) + ":hello-world-test~i-spy", // 1 client -> attached
		"4:10:" + strconv.Itoa(created) + ":multi-client",          // 10 clients -> attached
		"1:0:" + strconv.Itoa(created) + ":plain",                  // detached
		"2:0:0:zero-created",                                       // created 0 -> omitted
		"1:0:notanint:bad-created",                                 // unparseable created -> omitted
		"5:1:" + strconv.Itoa(created) + ":has:colons:in:name",     // ':' kept in name
		"",                             // blank -> skipped
		"  ",                           // whitespace -> skipped
		"1:0:" + strconv.Itoa(created), // too few fields -> skipped
	}, "\n")

	got := parseMCPSessions(output)
	want := []FleetSession{
		{Session: "hello-world-test~i-spy", Windows: 3, CreatedAt: wantCreated, Attached: true},
		{Session: "multi-client", Windows: 4, CreatedAt: wantCreated, Attached: true},
		{Session: "plain", Windows: 1, CreatedAt: wantCreated, Attached: false},
		{Session: "zero-created", Windows: 2, CreatedAt: "", Attached: false},
		{Session: "bad-created", Windows: 1, CreatedAt: "", Attached: false},
		{Session: "has:colons:in:name", Windows: 5, CreatedAt: wantCreated, Attached: true},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Empty input yields a non-nil empty slice (JSON-marshals to [], not null) —
	// the "no sessions => empty array" acceptance criterion.
	empty := parseMCPSessions("")
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty input: want non-nil empty slice, got %#v", empty)
	}
}

// TestMCPSessionListNotRunning asserts fleet_session_list rejects a stopped
// instance with a clear "not running" tool error rather than shelling in.
func TestMCPSessionListNotRunning(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cs := mcpConnect(t, newService())

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "fleet_session_list", Arguments: map[string]any{"fleet": "alpha", "instance": "i1"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "not running") {
		t.Fatalf("want 'not running' tool error, got IsError=%v text=%q", res.IsError, toolText(res))
	}
}

// TestStartMCPServerWritesPortFile checks the live wiring: a bound server records
// its port in ~/.fleet/mcp.port.
func TestStartMCPServerWritesPortFile(t *testing.T) {
	isolateFleetDir(t)
	// Serve creates ~/.fleet before startMCPServer; mirror that precondition.
	if err := os.MkdirAll(fleetpaths.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir fleet dir: %v", err)
	}
	httpSrv, port := startMCPServer(newService())
	if httpSrv == nil {
		t.Fatal("startMCPServer returned nil (could not bind)")
	}
	defer func() { _ = httpSrv.Shutdown(context.Background()) }()

	if port < mcpDefaultPort {
		t.Fatalf("bound port %d below default %d", port, mcpDefaultPort)
	}
	data, err := os.ReadFile(fleetpaths.McpPortPath())
	if err != nil {
		t.Fatalf("read mcp.port: %v", err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(port) {
		t.Fatalf("mcp.port = %q, want %d", string(data), port)
	}
}

// TestMCPRequiresBearerToken verifies the loopback endpoint rejects requests
// without the token and accepts them with it — the per-user access boundary.
func TestMCPRequiresBearerToken(t *testing.T) {
	isolateFleetDir(t)
	if err := os.MkdirAll(fleetpaths.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir fleet dir: %v", err)
	}
	httpSrv, port := startMCPServer(newService())
	if httpSrv == nil {
		t.Fatal("startMCPServer returned nil")
	}
	defer func() { _ = httpSrv.Shutdown(context.Background()) }()

	tokenBytes, err := os.ReadFile(fleetpaths.McpTokenPath())
	if err != nil {
		t.Fatalf("read mcp.token: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if len(token) < 32 {
		t.Fatalf("token too short: %q", token)
	}

	url := "http://127.0.0.1:" + strconv.Itoa(port)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	post := func(auth string) int {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(""); code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}
	if code := post("Bearer wrong-token"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", code)
	}
	if code := post("Bearer " + token); code == http.StatusUnauthorized {
		t.Fatalf("valid token rejected with 401")
	}
}

// TestMCPTokenPersistsAndEnvInjected verifies the token survives a restart and
// that mcp.env + the ~/.bashrc source line are written for mcp.json convenience.
func TestMCPTokenPersistsAndEnvInjected(t *testing.T) {
	isolateFleetDir(t)
	if err := os.MkdirAll(fleetpaths.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir fleet dir: %v", err)
	}

	srv1, port1 := startMCPServer(newService())
	if srv1 == nil {
		t.Fatal("startMCPServer returned nil")
	}
	tok1, _ := os.ReadFile(fleetpaths.McpTokenPath())
	_ = srv1.Shutdown(context.Background())

	// Env snippet exports the live endpoint + token.
	env, err := os.ReadFile(fleetpaths.McpEnvPath())
	if err != nil {
		t.Fatalf("read mcp.env: %v", err)
	}
	for _, want := range []string{
		"export FLEET_MCP_PORT=" + strconv.Itoa(port1),
		"export FLEET_MCP_URL=http://127.0.0.1:" + strconv.Itoa(port1),
		"export FLEET_MCP_TOKEN=" + strings.TrimSpace(string(tok1)),
	} {
		if !strings.Contains(string(env), want) {
			t.Fatalf("mcp.env missing %q:\n%s", want, env)
		}
	}

	// ~/.bashrc is wired to source it.
	bashrc, err := os.ReadFile(os.Getenv("HOME") + "/.bashrc")
	if err != nil || !strings.Contains(string(bashrc), mcpBashrcMarker) {
		t.Fatalf("~/.bashrc not wired to source mcp.env (err=%v): %s", err, bashrc)
	}

	// A second start reuses the same token.
	srv2, _ := startMCPServer(newService())
	if srv2 == nil {
		t.Fatal("second startMCPServer returned nil")
	}
	defer func() { _ = srv2.Shutdown(context.Background()) }()
	tok2, _ := os.ReadFile(fleetpaths.McpTokenPath())
	if strings.TrimSpace(string(tok1)) != strings.TrimSpace(string(tok2)) || len(tok2) == 0 {
		t.Fatalf("token not persisted across restart: %q vs %q", tok1, tok2)
	}
}
