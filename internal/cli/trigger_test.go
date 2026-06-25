package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestTriggerCreateSchedule(t *testing.T) {
	seed := fleet.FleetSettings{Agents: []fleet.Agent{{Name: "builder", Backend: fleet.BackendDevcontainer}}}
	gotFleet, result := stubMutate(t, seed)

	out, err := runCLI(t, "trigger", "create", "alpha", "nightly", "--agent", "builder", "--cron", "0 0 * * *", "--prompt", "go")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if *gotFleet != "alpha" {
		t.Errorf("fleet = %q", *gotFleet)
	}
	if len(result.Triggers) != 1 {
		t.Fatalf("want 1 trigger, got %+v", result.Triggers)
	}
	tr := result.Triggers[0]
	if tr.Name != "nightly" || tr.Type != fleet.TriggerSchedule || tr.Cron != "0 0 * * *" {
		t.Errorf("trigger = %+v", tr)
	}
	if len(tr.AgentNames) != 1 || tr.AgentNames[0] != "builder" {
		t.Errorf("agents = %v", tr.AgentNames)
	}
	if !strings.Contains(out, "Created trigger") {
		t.Errorf("output = %q", out)
	}
}

func TestTriggerCreateWebhook(t *testing.T) {
	seed := fleet.FleetSettings{Agents: []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}}
	_, result := stubMutate(t, seed)

	_, err := runCLI(t, "trigger", "create", "alpha", "hook",
		"--type", "webhook", "--agent", "a",
		"--webhook-name", "ci", "--filter-type", "jsonpath",
		"--json-path", "$.action", "--json-value", "opened")
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	tr := result.Triggers[0]
	if tr.Type != fleet.TriggerWebhook || tr.WebhookName != "ci" || tr.JSONPath != "$.action" || tr.JSONValue != "opened" {
		t.Errorf("webhook trigger = %+v", tr)
	}
	// Cron is a schedule-only field; a webhook trigger must not carry one.
	if tr.Cron != "" {
		t.Errorf("webhook trigger carries cron %q", tr.Cron)
	}
}

func TestTriggerCreateBash(t *testing.T) {
	seed := fleet.FleetSettings{Agents: []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}}
	_, result := stubMutate(t, seed)

	_, err := runCLI(t, "trigger", "create", "alpha", "poll",
		"--type", "bash", "--agent", "a",
		"--cron", "*/5 * * * *", "--script", "test -s /var/queue", "--prompt", "drain")
	if err != nil {
		t.Fatalf("create bash: %v", err)
	}
	tr := result.Triggers[0]
	if tr.Type != fleet.TriggerBash || tr.Cron != "*/5 * * * *" || tr.Script != "test -s /var/queue" {
		t.Errorf("bash trigger = %+v", tr)
	}
	// Bash triggers carry no webhook fields.
	if tr.WebhookName != "" || tr.Regex != "" {
		t.Errorf("bash trigger carries webhook fields: %+v", tr)
	}
}

func TestTriggerCreateRequiresAgent(t *testing.T) {
	stubMutate(t, fleet.FleetSettings{Agents: []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}})
	// No --agent: NormalizeTrigger rejects a trigger that activates no agents.
	if _, err := runCLI(t, "trigger", "create", "alpha", "t", "--cron", "* * * * *"); err == nil {
		t.Fatal("want error for a trigger with no agents")
	}
}

func TestTriggerCreateUnknownAgentFails(t *testing.T) {
	stubMutate(t, fleet.FleetSettings{Agents: []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}})
	if _, err := runCLI(t, "trigger", "create", "alpha", "t", "--agent", "ghost", "--cron", "* * * * *"); err == nil {
		t.Fatal("want unknown-agent error")
	}
}

func TestTriggerEditChangedOnly(t *testing.T) {
	seed := fleet.FleetSettings{
		Agents:   []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
		Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Prompt: "keep", Cron: "0 0 * * *"}},
	}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "trigger", "edit", "alpha", "t", "--cron", "5 4 * * *"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	tr := result.Triggers[0]
	if tr.Cron != "5 4 * * *" {
		t.Errorf("cron = %q, want updated", tr.Cron)
	}
	if tr.Prompt != "keep" {
		t.Errorf("prompt = %q, want unchanged", tr.Prompt)
	}
	if len(tr.AgentNames) != 1 || tr.AgentNames[0] != "a" {
		t.Errorf("agents changed unexpectedly: %v", tr.AgentNames)
	}
}

func TestTriggerEditReplaceAgents(t *testing.T) {
	seed := fleet.FleetSettings{
		Agents: []fleet.Agent{
			{Name: "a", Backend: fleet.BackendDevcontainer},
			{Name: "b", Backend: fleet.BackendDevcontainer},
		},
		Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "trigger", "edit", "alpha", "t", "--agent", "a", "--agent", "b"); err != nil {
		t.Fatalf("edit agents: %v", err)
	}
	if got := result.Triggers[0].AgentNames; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("agents = %v, want [a b]", got)
	}
}

func TestTriggerEditTypeChange(t *testing.T) {
	seed := fleet.FleetSettings{
		Agents:   []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
		Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 0 * * *"}},
	}
	_, result := stubMutate(t, seed)

	// Convert the schedule trigger to a webhook: the webhook fields are set and
	// the schedule-only cron is cleared by normalization.
	_, err := runCLI(t, "trigger", "edit", "alpha", "t",
		"--type", "webhook", "--webhook-name", "ci",
		"--filter-type", "jsonpath", "--json-path", "$.action", "--json-value", "opened")
	if err != nil {
		t.Fatalf("edit type change: %v", err)
	}
	tr := result.Triggers[0]
	if tr.Type != fleet.TriggerWebhook || tr.WebhookName != "ci" || tr.JSONPath != "$.action" || tr.JSONValue != "opened" {
		t.Errorf("webhook fields wrong after type change: %+v", tr)
	}
	if tr.Cron != "" {
		t.Errorf("schedule-only cron should be cleared after switch to webhook: %q", tr.Cron)
	}
}

func TestTriggerEditRename(t *testing.T) {
	seed := fleet.FleetSettings{
		Agents:   []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
		Triggers: []fleet.Trigger{{Name: "old", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "trigger", "edit", "alpha", "old", "--rename", "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if result.Triggers[0].Name != "new" {
		t.Errorf("trigger not renamed: %+v", result.Triggers)
	}
}

func TestTriggerDelete(t *testing.T) {
	seed := fleet.FleetSettings{
		Agents:   []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
		Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	}
	_, result := stubMutate(t, seed)

	if _, err := runCLI(t, "trigger", "delete", "alpha", "t"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(result.Triggers) != 0 {
		t.Errorf("trigger not deleted: %+v", result.Triggers)
	}
}

func TestTriggerList(t *testing.T) {
	orig := loadAutomation
	t.Cleanup(func() { loadAutomation = orig })
	loadAutomation = func(_ context.Context, _ string) (fleet.FleetSettings, error) {
		return fleet.FleetSettings{
			Triggers: []fleet.Trigger{
				{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"builder"}, Cron: "0 0 * * *"},
				{Name: "hook", Type: fleet.TriggerWebhook, AgentNames: []string{"a", "b"}, WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: "push"},
				{Name: "poll", Type: fleet.TriggerBash, AgentNames: []string{"a"}, Cron: "*/5 * * * *", Script: "test -s /q"},
			},
		}, nil
	}

	out, err := runCLI(t, "trigger", "ls", "alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"NAME", "TYPE", "AGENTS", "nightly", "0 0 * * *", "hook", "webhook:ci", "a,b", "poll", "bash", "sh: test -s /q"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestTriggerLogs(t *testing.T) {
	orig := triggerLogs
	t.Cleanup(func() { triggerLogs = orig })
	var gotFleet, gotTrigger string
	triggerLogs = func(_ context.Context, f, tr string) (string, error) {
		gotFleet, gotTrigger = f, tr
		return "===== event-20260623T090000Z.log =====\nhello payload\n", nil
	}

	out, err := runCLI(t, "trigger", "logs", "alpha", "nightly")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if gotFleet != "alpha" || gotTrigger != "nightly" {
		t.Errorf("fetched logs for %q/%q, want alpha/nightly", gotFleet, gotTrigger)
	}
	if !strings.Contains(out, "hello payload") {
		t.Errorf("output missing payload:\n%s", out)
	}
}

func TestTriggerLogsEmpty(t *testing.T) {
	orig := triggerLogs
	t.Cleanup(func() { triggerLogs = orig })
	triggerLogs = func(context.Context, string, string) (string, error) { return "", nil }

	out, err := runCLI(t, "trigger", "logs", "alpha", "nightly")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(out, "No events recorded") {
		t.Errorf("empty history output = %q", out)
	}
}

func TestTriggerAliases(t *testing.T) {
	cmds := map[string]string{}
	for _, c := range newTriggerCmd().Commands() {
		for _, a := range c.Aliases {
			cmds[a] = c.Name()
		}
	}
	if cmds["ls"] != "list" || cmds["rm"] != "delete" {
		t.Errorf("trigger aliases = %v, want ls->list, rm->delete", cmds)
	}
}
