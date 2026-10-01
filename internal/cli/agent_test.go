package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
	mutateAutomation = func(_ context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) error {
		*gotFleet = fleetName
		out, err := fn(seed)
		if err != nil {
			return err
		}
		*result = out
		return nil
	}
	return gotFleet, result
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

	out, err := runCLI(t, "agent", "list", "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"NAME", "BACKEND", "TRIGGERS", "builder", "devcontainer", "claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
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
