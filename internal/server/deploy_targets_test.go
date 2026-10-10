package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// stubDaemonHost makes the daemon host have exactly the given binaries on its
// PATH and the given name.
func stubDaemonHost(t *testing.T, host string, binaries ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, b := range binaries {
		have[b] = true
	}
	origLook, origHost := daemonLookPath, daemonHostname
	daemonLookPath = func(file string) (string, error) {
		if have[file] {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
	daemonHostname = func() (string, error) { return host, nil }
	t.Cleanup(func() { daemonLookPath, daemonHostname = origLook, origHost })
}

func rememberBackend(t *testing.T, backendType fleet.BackendType) {
	t.Helper()
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(backendType)}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

func storedDefaultBackend(t *testing.T) string {
	t.Helper()
	cfg, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg.DefaultBackend
}

func listTargets(t *testing.T, svc *service, fleetName string) *fleetgrpc.ListDeployTargetsReply {
	t.Helper()
	reply, err := svc.ListDeployTargets(context.Background(), &fleetgrpc.ListDeployTargetsRequest{Fleet: fleetName})
	if err != nil {
		t.Fatalf("ListDeployTargets: %v", err)
	}
	return reply
}

// Availability is the daemon host's: each target needs its CLI on fleetd's
// PATH, and a missing one is reported with the reason rather than dropped.
func TestListDeployTargetsJudgesToolsOnTheDaemonHost(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonHost(t, "fleetd-host", "docker", "devcontainer", "gh")

	reply := listTargets(t, newService(), "")

	type row struct {
		backend   fleetgrpc.BackendType
		available bool
	}
	want := []row{
		{fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER, true},
		{fleetgrpc.BackendType_BACKEND_TYPE_CODER, false},
		{fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES, true},
	}
	if len(reply.GetTargets()) != len(want) {
		t.Fatalf("targets = %v, want %d entries", reply.GetTargets(), len(want))
	}
	for i, w := range want {
		got := reply.GetTargets()[i]
		if got.GetTarget().GetBackend() != w.backend || got.GetAvailable() != w.available {
			t.Errorf("target %d = %v, want backend %v available %v", i, got, w.backend, w.available)
		}
		if got.GetAvailable() != (got.GetReason() == "") {
			t.Errorf("target %d: reason %q must be set exactly when unavailable", i, got.GetReason())
		}
	}
	if reason := reply.GetTargets()[1].GetReason(); !strings.Contains(reason, "coder") {
		t.Errorf("coder reason %q should name the missing CLI", reason)
	}
	if got := reply.GetDefaultTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER {
		t.Errorf("default with nothing remembered = %v, want devcontainer", got)
	}
	if reply.GetHost() != "fleetd-host" {
		t.Errorf("host = %q, want fleetd-host", reply.GetHost())
	}
	tools := map[string]bool{}
	for _, tool := range reply.GetHostTools() {
		tools[tool.GetBinary()] = tool.GetFound()
	}
	if len(tools) != 2 || !tools["docker"] || !tools["devcontainer"] {
		t.Errorf("host tools = %v, want docker and devcontainer, both found", reply.GetHostTools())
	}

	// With devcontainer missing on the daemon host, its target and its tool row
	// both say so.
	stubDaemonHost(t, "fleetd-host", "docker")
	reply = listTargets(t, newService(), "")
	if reply.GetTargets()[0].GetAvailable() {
		t.Error("devcontainer target available without the devcontainer CLI on fleetd's PATH")
	}
	for _, tool := range reply.GetHostTools() {
		if tool.GetBinary() == "devcontainer" && tool.GetFound() {
			t.Error("devcontainer host tool reported found")
		}
	}
}

func TestListDeployTargetsReportsTheRememberedDefault(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonHost(t, "h", "devcontainer", "coder", "gh")
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"git":      {Name: "git", Remote: "git@x:a.git"},
		"template": {Name: "template", Remote: "file:///srv/template"},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	svc := newService()

	rememberBackend(t, fleet.BackendCodespaces)
	if got := listTargets(t, svc, "git").GetDefaultTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES {
		t.Errorf("git fleet default = %v, want the remembered codespaces", got)
	}
	// An unknown fleet is a create for a new one: no narrowing.
	if got := listTargets(t, svc, "brand-new").GetDefaultTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES {
		t.Errorf("unknown fleet default = %v, want the remembered codespaces", got)
	}

	// A template fleet only offers devcontainer, and its default narrows to it.
	reply := listTargets(t, svc, "template")
	if len(reply.GetTargets()) != 1 || reply.GetTargets()[0].GetTarget().GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER {
		t.Errorf("template fleet targets = %v, want devcontainer only", reply.GetTargets())
	}
	if got := reply.GetDefaultTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER {
		t.Errorf("template fleet default = %v, want devcontainer", got)
	}

	// A value this build doesn't know is no default at all.
	if err := state.SaveConfig(&state.Config{DefaultBackend: "bogus"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if got := listTargets(t, svc, "git").GetDefaultTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER {
		t.Errorf("default with an unknown remembered backend = %v, want devcontainer", got)
	}
}

// createAndDrain runs one create to completion and returns its JobStarted.
func createAndDrain(t *testing.T, client fleetgrpc.FleetServiceClient, req *fleetgrpc.CreateInstanceRequest) *fleetgrpc.JobStarted {
	t.Helper()
	stream, err := client.CreateInstance(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateInstance(%s): %v", req.GetInstance(), err)
	}
	evs := drainJob(t, stream)
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("CreateInstance(%s) did not succeed: %v", req.GetInstance(), evs[len(evs)-1])
	}
	return evs[0].GetStarted()
}

func recordedBackend(t *testing.T, fleetName, instanceName string) fleet.BackendType {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	inst, err := st.Fleets[fleetName].GetInstance(instanceName)
	if err != nil {
		t.Fatalf("instance %s/%s: %v", fleetName, instanceName, err)
	}
	return inst.Backend
}

// remember_default makes the resolved target the one later unspecified creates
// get; creates without it never move the default; JobStarted names the target.
func TestCreateInstanceRemembersAndUsesTheDefaultTarget(t *testing.T) {
	isolateFleetDir(t)
	stubCreateJob(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	// Nothing remembered: unspecified means devcontainer.
	started := createAndDrain(t, client, &fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: "i0"})
	if got := started.GetTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER {
		t.Fatalf("JobStarted.target = %v, want devcontainer", got)
	}
	if got := storedDefaultBackend(t); got != "" {
		t.Fatalf("a create without remember_default stored default %q", got)
	}

	started = createAndDrain(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "i1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES, RememberDefault: true,
	})
	if got := started.GetTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES {
		t.Fatalf("JobStarted.target = %v, want codespaces", got)
	}
	if got := storedDefaultBackend(t); got != string(fleet.BackendCodespaces) {
		t.Fatalf("remembered default = %q, want codespaces", got)
	}

	// An explicit target without remember_default doesn't move the default.
	createAndDrain(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "i2", Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODER,
	})
	if got := storedDefaultBackend(t); got != string(fleet.BackendCodespaces) {
		t.Fatalf("remembered default after an explicit coder create = %q, want codespaces", got)
	}

	// An unspecified create (fleet up without --backend, MCP without backend)
	// takes the remembered target.
	started = createAndDrain(t, client, &fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: "i3"})
	if got := started.GetTarget().GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES {
		t.Fatalf("unspecified create JobStarted.target = %v, want the remembered codespaces", got)
	}
	if got := recordedBackend(t, "alpha", "i3"); got != fleet.BackendCodespaces {
		t.Fatalf("unspecified create recorded backend %q, want codespaces", got)
	}

	// A rejected create remembers nothing.
	if err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "i3", Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER, RememberDefault: true,
	}); err == nil {
		t.Fatal("duplicate create should be rejected")
	}
	if got := storedDefaultBackend(t); got != string(fleet.BackendCodespaces) {
		t.Fatalf("a rejected create changed the remembered default to %q", got)
	}
}

// Every client sends its whole Config to SetConfig. A copy fetched before
// another client remembered a target must not undo it.
func TestSetConfigKeepsRememberedDefaultTarget(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	if err := svc.rememberDefaultTarget(fleet.DeployTarget{Backend: fleet.BackendCodespaces}); err != nil {
		t.Fatalf("rememberDefaultTarget: %v", err)
	}
	for _, sent := range []fleetgrpc.BackendType{
		fleetgrpc.BackendType_BACKEND_TYPE_CODER,       // a stale copy
		fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED, // a client that never saw a default
	} {
		_, err := svc.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{
			Agent:          &fleetgrpc.AgentSettings{ToolSelection: "claude"},
			DefaultBackend: sent,
		}})
		if err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		if got := storedDefaultBackend(t); got != string(fleet.BackendCodespaces) {
			t.Fatalf("SetConfig carrying %v changed the remembered default to %q", sent, got)
		}
	}
}

// Remote clients get the same deploy targets as local ones: the RPC passes the
// tunnel's bearer check and is not local-only.
func TestTunnelServesListDeployTargets(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonHost(t, "remote-host", "devcontainer")
	if localOnlyMethods[fleetgrpc.FleetService_ListDeployTargets_FullMethodName] {
		t.Fatal("ListDeployTargets must not be local-only")
	}
	const token = "test-token"
	authUnary, authStream := bearerAuthInterceptors(token)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(gs, newService())
	client := dialBuf(t, gs)

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	reply, err := client.ListDeployTargets(ctx, &fleetgrpc.ListDeployTargetsRequest{})
	if err != nil {
		t.Fatalf("ListDeployTargets over the tunnel: %v", err)
	}
	if reply.GetHost() != "remote-host" || len(reply.GetTargets()) == 0 {
		t.Fatalf("tunnel reply = %v", reply)
	}
}

// fleet_up's reply names where the instance went, and says when the daemon
// picked it because the caller left backend out.
func TestMCPUpReplyNamesTheResolvedTarget(t *testing.T) {
	isolateFleetDir(t)
	stubCreateJob(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	rememberBackend(t, fleet.BackendCodespaces)
	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)

	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "i1"}, &out)
	if out.Target == nil || out.Target.Backend != "codespaces" || !out.Target.Defaulted {
		t.Fatalf("async reply target = %+v, want the remembered codespaces, defaulted", out.Target)
	}
	if js := pollJobStatus(t, cs, out.JobID); js.State != "succeeded" {
		t.Fatalf("job state = %q", js.State)
	}

	out = FleetJobOutput{}
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "i2", "backend": "devcontainer", "wait": true}, &out)
	if out.Target == nil || out.Target.Backend != "devcontainer" || out.Target.Defaulted {
		t.Fatalf("wait reply target = %+v, want devcontainer, not defaulted", out.Target)
	}
	if got := storedDefaultBackend(t); got != string(fleet.BackendCodespaces) {
		t.Fatalf("fleet_up changed the remembered default to %q", got)
	}
}

// The fleet_up schema tells an agent that an omitted backend is the remembered
// default, not devcontainer.
func TestMCPUpSchemaNamesTheRememberedDefault(t *testing.T) {
	isolateFleetDir(t)
	cs := mcpConnect(t, newService())
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "fleet_up" {
			continue
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		if !strings.Contains(string(schema), "remembered default deploy target") {
			t.Errorf("fleet_up backend schema does not name the remembered default: %s", schema)
		}
		if strings.Contains(string(schema), "devcontainer (default)") {
			t.Errorf("fleet_up schema still says devcontainer is the default: %s", schema)
		}
		if !strings.Contains(tool.Description, "remembered default deploy target") {
			t.Errorf("fleet_up description does not name the remembered default: %s", tool.Description)
		}
		return
	}
	t.Fatal("fleet_up not registered")
}
