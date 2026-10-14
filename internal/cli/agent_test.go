package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// stubMutate replaces the mutateAutomation seam with one that runs the command's
// mutation against seed (in memory) and records the fleet name + result, so the
// command + mutation logic is exercised without a server. The captured result
// pointer is updated on success.
func stubMutate(t *testing.T, seed fleet.FleetSettings) (gotFleet *string, result *fleet.FleetSettings) {
	t.Helper()
	gotFleet = new(string)
	result = new(fleet.FleetSettings)
	orig := mutateAutomation
	t.Cleanup(func() { mutateAutomation = orig })
	mutateAutomation = func(_ context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) ([]string, error) {
		*gotFleet = fleetName
		out, err := fn(seed)
		if err != nil {
			return nil, err
		}
		*result = out
		return nil, nil
	}
	return gotFleet, result
}

// The daemon's warnings about a saved agent (its login check will refuse
// its firings) reach the user on stderr, after the confirmation.
func TestAgentCreatePrintsTheDaemonsWarnings(t *testing.T) {
	orig := mutateAutomation
	t.Cleanup(func() { mutateAutomation = orig })
	mutateAutomation = func(_ context.Context, _ string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) ([]string, error) {
		if _, err := fn(fleet.FleetSettings{}); err != nil {
			return nil, err
		}
		return []string{"agent w: log in to Claude once for this fleet on ☸ prod; until then its firings are refused"}, nil
	}
	cmd := NewRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"agent", "create", "alpha", "w"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `Created agent "w"`) {
		t.Errorf("stdout = %q", stdout.String())
	}
	if want := "warning: agent w: log in to Claude once for this fleet on ☸ prod; until then its firings are refused\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestAgentCreate(t *testing.T) {
	gotFleet, result := stubMutate(t, fleet.FleetSettings{})

	out, err := runCLI(t, "agent", "create", "alpha", "builder", "--backend", "coder", "--system-prompt", "be terse")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if *gotFleet != "alpha" {
		t.Errorf("fleet = %q, want alpha", *gotFleet)
	}
	if len(result.Agents) != 1 {
		t.Fatalf("want 1 agent, got %+v", result.Agents)
	}
	a := result.Agents[0]
	if a.Name != "builder" || a.Backend != fleet.BackendCoder || a.SystemPrompt != "be terse" {
		t.Errorf("agent = %+v", a)
	}
	// An unspecified command normalizes to the default.
	if a.Command != strings.TrimSpace(fleet.DefaultAgentCommand) {
		t.Errorf("command = %q, want default", a.Command)
	}
	if !strings.Contains(out, "Created agent") {
		t.Errorf("output = %q", out)
	}
}

func TestAgentCreateDuplicateFails(t *testing.T) {
	stubMutate(t, fleet.FleetSettings{Agents: []fleet.Agent{{Name: "builder", Backend: fleet.BackendDevcontainer}}})
	if _, err := runCLI(t, "agent", "create", "alpha", "builder"); err == nil {
		t.Fatal("want duplicate-name error")
	}
}

func TestAgentEditChangedOnly(t *testing.T) {
	seed := fleet.FleetSettings{Agents: []fleet.Agent{
		{Name: "a", Backend: fleet.BackendDevcontainer, Command: "orig", SystemPrompt: "keep me"},
	}}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "agent", "edit", "alpha", "a", "--command", "new"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	a := result.Agents[0]
	if a.Command != "new" {
		t.Errorf("command = %q, want new", a.Command)
	}
	if a.SystemPrompt != "keep me" {
		t.Errorf("system prompt = %q, want unchanged", a.SystemPrompt)
	}
}

func TestAgentEditBackend(t *testing.T) {
	seed := fleet.FleetSettings{Agents: []fleet.Agent{
		{Name: "a", Backend: fleet.BackendDevcontainer, Command: "claude", SystemPrompt: "keep me"},
	}}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "agent", "edit", "alpha", "a", "--backend", "coder"); err != nil {
		t.Fatalf("edit backend: %v", err)
	}
	a := result.Agents[0]
	if a.Backend != fleet.BackendCoder {
		t.Errorf("backend = %q, want coder", a.Backend)
	}
	// Other fields are untouched (only-passed-flags-change).
	if a.Command != "claude" || a.SystemPrompt != "keep me" {
		t.Errorf("unrelated fields changed: %+v", a)
	}
}

func TestAgentEditRenameRewritesTriggers(t *testing.T) {
	seed := fleet.FleetSettings{
		Agents:   []fleet.Agent{{Name: "old", Backend: fleet.BackendDevcontainer}},
		Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"old"}, Cron: "* * * * *"}},
	}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "agent", "edit", "alpha", "old", "--rename", "new"); err != nil {
		t.Fatalf("edit rename: %v", err)
	}
	if result.Agents[0].Name != "new" {
		t.Errorf("agent not renamed: %+v", result.Agents)
	}
	if got := result.Triggers[0].AgentNames; len(got) != 1 || got[0] != "new" {
		t.Errorf("trigger ref not rewritten: %v", got)
	}
}

func TestAgentDelete(t *testing.T) {
	seed := fleet.FleetSettings{Agents: []fleet.Agent{
		{Name: "a", Backend: fleet.BackendDevcontainer},
		{Name: "b", Backend: fleet.BackendDevcontainer},
	}}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "agent", "rm", "alpha", "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(result.Agents) != 1 || result.Agents[0].Name != "b" {
		t.Errorf("want [b] left, got %+v", result.Agents)
	}
}

func TestAgentDeleteReferencedFails(t *testing.T) {
	stubMutate(t, fleet.FleetSettings{
		Agents:   []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
		Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	})
	if _, err := runCLI(t, "agent", "delete", "alpha", "a"); err == nil {
		t.Fatal("want referenced-agent error")
	}
}

func TestAgentList(t *testing.T) {
	orig := loadAutomation
	t.Cleanup(func() { loadAutomation = orig })
	loadAutomation = func(_ context.Context, fleetName string) (fleet.FleetSettings, error) {
		return fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "builder", Backend: fleet.BackendDevcontainer, Command: "claude"}},
			Triggers: []fleet.Trigger{{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"builder"}, Cron: "0 0 * * *"}},
		}, nil
	}

	origStatus := loadAutomationStatus
	t.Cleanup(func() { loadAutomationStatus = origStatus })
	loadAutomationStatus = func(_ context.Context, fleetName string) (*fleetgrpc.AutomationStatus, error) {
		return &fleetgrpc.AutomationStatus{Agents: []*fleetgrpc.AutomationAgentStatus{
			{Fleet: fleetName, Agent: "nightly-k8s", Queued: 2, Running: 1, Login: "log in to Claude once for this fleet on ☸ prod", LoginTool: "claude"},
		}}, nil
	}

	out, err := runCLI(t, "agent", "list", "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"NAME", "BACKEND", "TRIGGERS", "STATUS", "builder", "devcontainer", "claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`builder\s+devcontainer\s+1\s+-\s+claude`).MatchString(out) {
		t.Errorf("builder's row has no status:\n%s", out)
	}
}

// The STATUS column says what the scheduler holds of an agent: its runs
// running and queued, and why the login check refuses its firings.
func TestAgentListShowsTheAutomationStatus(t *testing.T) {
	origLoad, origStatus := loadAutomation, loadAutomationStatus
	t.Cleanup(func() { loadAutomation, loadAutomationStatus = origLoad, origStatus })
	loadAutomation = func(context.Context, string) (fleet.FleetSettings, error) {
		return fleet.FleetSettings{Agents: []fleet.Agent{{Name: "nightly", Backend: fleet.BackendKubernetes, KubeCluster: "prod-abc234", Command: "claude"}}}, nil
	}
	loadAutomationStatus = func(_ context.Context, fleetName string) (*fleetgrpc.AutomationStatus, error) {
		return &fleetgrpc.AutomationStatus{Agents: []*fleetgrpc.AutomationAgentStatus{
			{Fleet: fleetName, Agent: "nightly", Queued: 2, Running: 1, Login: "log in to Claude once for this fleet on ☸ prod", LoginTool: "claude"},
		}}, nil
	}
	out, err := runCLI(t, "agent", "list", "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "1 running, 2 queued, log in to Claude once for this fleet on ☸ prod") {
		t.Fatalf("list output:\n%s", out)
	}
}

func TestAgentAliases(t *testing.T) {
	cmds := map[string]string{}
	for _, c := range newAgentCmd().Commands() {
		for _, a := range c.Aliases {
			cmds[a] = c.Name()
		}
	}
	if cmds["ls"] != "list" || cmds["rm"] != "delete" {
		t.Errorf("agent aliases = %v, want ls->list, rm->delete", cmds)
	}
}

// stubAgentClusters resolves --cluster references against id -> refs without
// a daemon ("" is the only connection, only), recording each lookup.
func stubAgentClusters(t *testing.T, only string, ids map[string]string) *[]string {
	t.Helper()
	var asked []string
	orig := agentClusterID
	agentClusterID = func(_ context.Context, ref string) (string, error) {
		asked = append(asked, ref)
		if ref == "" {
			if only == "" {
				return "", errors.New("2 Kubernetes connections (a, b): name one")
			}
			return only, nil
		}
		if id, ok := ids[ref]; ok {
			return id, nil
		}
		return "", fmt.Errorf("no Kubernetes connection %q", ref)
	}
	t.Cleanup(func() { agentClusterID = orig })
	return &asked
}

// --cluster stores the connection's id and implies kubernetes; kubernetes
// with no --cluster is the only connection; no flag at all is devcontainer.
func TestAgentCreateOnACluster(t *testing.T) {
	stubAgentClusters(t, "minikube-abc234", map[string]string{"dev": "minikube-abc234", "prod": "prod-def567"})
	for _, tt := range []struct {
		args []string
		want fleet.DeployTarget
		err  string
	}{
		{[]string{"--cluster", "prod"}, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "prod-def567"}, ""},
		{[]string{"--backend", "kubernetes"}, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "minikube-abc234"}, ""},
		{[]string{"--backend", "kubernetes", "--cluster", "dev"}, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "minikube-abc234"}, ""},
		{nil, fleet.DeployTarget{Backend: fleet.BackendDevcontainer}, ""},
		{[]string{"--backend", "coder", "--cluster", "dev"}, fleet.DeployTarget{}, "not coder"},
		{[]string{"--cluster", "nope"}, fleet.DeployTarget{}, `no Kubernetes connection "nope"`},
	} {
		_, result := stubMutate(t, fleet.FleetSettings{})
		_, err := runCLI(t, append([]string{"agent", "create", "alpha", "k"}, tt.args...)...)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%v: error = %v, want %q", tt.args, err, tt.err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		if got := result.Agents[0].DeployTarget(); got != tt.want {
			t.Errorf("%v: target %+v, want %+v", tt.args, got, tt.want)
		}
	}
}

// An edit moves an agent only with --backend or --cluster; kubernetes
// without --cluster keeps the agent's connection.
func TestAgentEditCluster(t *testing.T) {
	asked := stubAgentClusters(t, "", map[string]string{"prod": "prod-def567"})
	seed := fleet.FleetSettings{Agents: []fleet.Agent{{Name: "k", Backend: fleet.BackendKubernetes, KubeCluster: "minikube-abc234", Command: "claude"}}}
	for _, tt := range []struct {
		args []string
		want fleet.DeployTarget
	}{
		{[]string{"--command", "codex"}, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "minikube-abc234"}},
		{[]string{"--backend", "kubernetes"}, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "minikube-abc234"}},
		{[]string{"--cluster", "prod"}, fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "prod-def567"}},
		{[]string{"--backend", "devcontainer"}, fleet.DeployTarget{Backend: fleet.BackendDevcontainer}},
	} {
		_, result := stubMutate(t, seed)
		if _, err := runCLI(t, append([]string{"agent", "edit", "alpha", "k"}, tt.args...)...); err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		if got := result.Agents[0].DeployTarget(); got != tt.want {
			t.Errorf("%v: target %+v, want %+v", tt.args, got, tt.want)
		}
	}
	if len(*asked) != 1 || (*asked)[0] != "prod" {
		t.Errorf("looked up %q, want only the named prod", *asked)
	}
	// From another backend, kubernetes takes the only connection: there are
	// two here, so the edit asks for one.
	_, _ = stubMutate(t, fleet.FleetSettings{Agents: []fleet.Agent{{Name: "d", Backend: fleet.BackendDevcontainer}}})
	if _, err := runCLI(t, "agent", "edit", "alpha", "d", "--backend", "kubernetes"); err == nil || !strings.Contains(err.Error(), "name one") {
		t.Errorf("kubernetes with two connections: error = %v, want a request to name one", err)
	}
}

// The list's BACKEND column names a cluster agent's connection.
func TestAgentListShowsTheCluster(t *testing.T) {
	orig, origStatus := loadAutomation, loadAutomationStatus
	t.Cleanup(func() { loadAutomation, loadAutomationStatus = orig, origStatus })
	loadAutomationStatus = func(context.Context, string) (*fleetgrpc.AutomationStatus, error) {
		return &fleetgrpc.AutomationStatus{}, nil
	}
	loadAutomation = func(context.Context, string) (fleet.FleetSettings, error) {
		return fleet.FleetSettings{Agents: []fleet.Agent{{Name: "k", Backend: fleet.BackendKubernetes, KubeCluster: "prod-def567", Command: "claude"}}}, nil
	}
	out, err := runCLI(t, "agent", "list", "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "kubernetes:prod-def567") {
		t.Errorf("list should name the cluster:\n%s", out)
	}
}
