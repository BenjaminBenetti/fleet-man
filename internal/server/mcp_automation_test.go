package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// seedBareFleet persists a single fleet with no instances and no automation, so
// the automation tools have a fleet to mutate. Returns an MCP client session.
func seedBareFleet(t *testing.T) *mcp.ClientSession {
	t.Helper()
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@example.com:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return mcpConnect(t, newService())
}

// callErr calls a tool and returns its tool-error text, failing if the call
// unexpectedly succeeded (mirrors the error-path checks in mcp_test.go).
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s: expected a tool error, got %s", name, toolText(res))
	}
	return toolText(res)
}

func TestMCPAgentLifecycle(t *testing.T) {
	cs := seedBareFleet(t)

	// Create an agent; the returned config reflects it.
	var out AutomationOutput
	callJSON(t, cs, "fleet_agent_create", map[string]any{
		"fleet": "alpha", "name": "builder", "backend": "coder", "system_prompt": "be terse",
	}, &out)
	if len(out.Agents) != 1 || out.Agents[0].Name != "builder" || out.Agents[0].Backend != "coder" {
		t.Fatalf("create returned %+v", out.Agents)
	}
	// An unspecified command is normalized to the default (non-empty).
	if out.Agents[0].Command == "" {
		t.Errorf("command should default to non-empty, got empty")
	}

	// It survives a fresh read (persisted through SetFleetSettings).
	var listed AutomationOutput
	callJSON(t, cs, "fleet_automation_list", map[string]any{"fleet": "alpha"}, &listed)
	if len(listed.Agents) != 1 || listed.Agents[0].SystemPrompt != "be terse" {
		t.Fatalf("list returned %+v", listed.Agents)
	}

	// A duplicate name is a tool error.
	if msg := callErr(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "builder"}); !strings.Contains(msg, "already exists") {
		t.Errorf("duplicate error = %q", msg)
	}

	// Update only the backend; the command is preserved (empty == keep).
	var updated AutomationOutput
	callJSON(t, cs, "fleet_agent_update", map[string]any{"fleet": "alpha", "name": "builder", "backend": "devcontainer"}, &updated)
	if updated.Agents[0].Backend != "devcontainer" {
		t.Errorf("backend not updated: %+v", updated.Agents[0])
	}
	if updated.Agents[0].SystemPrompt != "be terse" {
		t.Errorf("system prompt should be preserved on partial update: %+v", updated.Agents[0])
	}

	// Delete it.
	var afterDelete AutomationOutput
	callJSON(t, cs, "fleet_agent_delete", map[string]any{"fleet": "alpha", "name": "builder"}, &afterDelete)
	if len(afterDelete.Agents) != 0 {
		t.Errorf("agent not deleted: %+v", afterDelete.Agents)
	}
}

func TestMCPTriggerLifecycleAndRefs(t *testing.T) {
	cs := seedBareFleet(t)

	callJSON(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "old"}, nil)

	// Create a schedule trigger referencing the agent.
	var out AutomationOutput
	callJSON(t, cs, "fleet_trigger_create", map[string]any{
		"fleet": "alpha", "name": "nightly", "type": "schedule",
		"agents": []string{"old"}, "cron": "0 0 * * *", "prompt": "go",
	}, &out)
	if len(out.Triggers) != 1 || out.Triggers[0].Cron != "0 0 * * *" {
		t.Fatalf("trigger create returned %+v", out.Triggers)
	}

	// Referencing an unknown agent is a tool error.
	callErr(t, cs, "fleet_trigger_create", map[string]any{
		"fleet": "alpha", "name": "bad", "type": "schedule", "agents": []string{"ghost"}, "cron": "* * * * *",
	})

	// Renaming the agent rewrites the trigger's reference.
	var renamed AutomationOutput
	callJSON(t, cs, "fleet_agent_update", map[string]any{"fleet": "alpha", "name": "old", "new_name": "new"}, &renamed)
	if renamed.Agents[0].Name != "new" {
		t.Fatalf("agent not renamed: %+v", renamed.Agents)
	}
	if got := renamed.Triggers[0].Agents; len(got) != 1 || got[0] != "new" {
		t.Fatalf("trigger ref not rewritten on rename: %v", got)
	}

	// The now-referenced agent cannot be deleted.
	if msg := callErr(t, cs, "fleet_agent_delete", map[string]any{"fleet": "alpha", "name": "new"}); !strings.Contains(msg, "referenced by trigger") {
		t.Errorf("delete-referenced error = %q", msg)
	}

	// Delete the trigger, then the agent is free.
	callJSON(t, cs, "fleet_trigger_delete", map[string]any{"fleet": "alpha", "name": "nightly"}, nil)
	var done AutomationOutput
	callJSON(t, cs, "fleet_agent_delete", map[string]any{"fleet": "alpha", "name": "new"}, &done)
	if len(done.Agents) != 0 || len(done.Triggers) != 0 {
		t.Fatalf("expected empty automation, got %+v", done)
	}
}

func TestMCPTriggerUpdatePartial(t *testing.T) {
	cs := seedBareFleet(t)
	callJSON(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "a"}, nil)
	callJSON(t, cs, "fleet_trigger_create", map[string]any{
		"fleet": "alpha", "name": "t", "type": "schedule", "agents": []string{"a"}, "cron": "0 0 * * *", "prompt": "keep",
	}, nil)

	// Change only the cron; the prompt and agents are preserved.
	var out AutomationOutput
	callJSON(t, cs, "fleet_trigger_update", map[string]any{"fleet": "alpha", "name": "t", "cron": "5 4 * * *"}, &out)
	tr := out.Triggers[0]
	if tr.Cron != "5 4 * * *" {
		t.Errorf("cron not updated: %q", tr.Cron)
	}
	if tr.Prompt != "keep" {
		t.Errorf("prompt should be preserved: %q", tr.Prompt)
	}
	if len(tr.Agents) != 1 || tr.Agents[0] != "a" {
		t.Errorf("agents should be preserved: %v", tr.Agents)
	}
}

// TestMCPBashTrigger covers the bash trigger through the MCP tools: create with a
// cron + script, then a partial update that changes only the script.
func TestMCPBashTrigger(t *testing.T) {
	cs := seedBareFleet(t)
	callJSON(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "a"}, nil)

	var out AutomationOutput
	callJSON(t, cs, "fleet_trigger_create", map[string]any{
		"fleet": "alpha", "name": "poll", "type": "bash",
		"agents": []string{"a"}, "cron": "*/5 * * * *", "script": "test -s /var/queue", "prompt": "drain",
	}, &out)
	if len(out.Triggers) != 1 {
		t.Fatalf("create returned %+v", out.Triggers)
	}
	tr := out.Triggers[0]
	if tr.Type != "bash" || tr.Cron != "*/5 * * * *" || tr.Script != "test -s /var/queue" {
		t.Fatalf("bash trigger fields wrong: %+v", tr)
	}

	// A bash trigger with an empty script is rejected (server-side normalize).
	if msg := callErr(t, cs, "fleet_trigger_create", map[string]any{
		"fleet": "alpha", "name": "bad", "type": "bash", "agents": []string{"a"}, "cron": "* * * * *",
	}); !strings.Contains(msg, "script") {
		t.Errorf("empty-script error = %q", msg)
	}

	// Partial update: change only the script; cron + prompt are preserved.
	var upd AutomationOutput
	callJSON(t, cs, "fleet_trigger_update", map[string]any{"fleet": "alpha", "name": "poll", "script": "curl -sf http://x | grep -q ok"}, &upd)
	got := upd.Triggers[0]
	if got.Script != "curl -sf http://x | grep -q ok" {
		t.Errorf("script not updated: %q", got.Script)
	}
	if got.Cron != "*/5 * * * *" || got.Prompt != "drain" {
		t.Errorf("cron/prompt should be preserved: %+v", got)
	}
}

func TestMCPAutomationUnknownFleet(t *testing.T) {
	cs := seedBareFleet(t)
	if msg := callErr(t, cs, "fleet_automation_list", map[string]any{"fleet": "ghost"}); !strings.Contains(msg, "not found") {
		t.Errorf("unknown-fleet error = %q", msg)
	}
	if msg := callErr(t, cs, "fleet_agent_create", map[string]any{"fleet": "ghost", "name": "x"}); !strings.Contains(msg, "not found") {
		t.Errorf("create unknown-fleet error = %q", msg)
	}
}

func TestMCPAutomationMissingArgs(t *testing.T) {
	cs := seedBareFleet(t)
	// Missing required name -> input-schema validation tool error.
	callErr(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha"})
	// Empty fleet -> our own guard.
	callErr(t, cs, "fleet_automation_list", map[string]any{"fleet": ""})
}

// TestMCPAutomationPreservesOtherSettings verifies a write through the
// automation tools does not clobber unrelated FleetSettings fields (the
// read-modify-write must carry them through).
func TestMCPAutomationPreservesOtherSettings(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{ClaudeCodeMount: true, HomeDir: "/home/node"}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cs := mcpConnect(t, newService())

	callJSON(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "a"}, nil)

	// Re-read raw state: the mount + home dir must still be set.
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := st.Fleets["alpha"].Settings
	if !got.ClaudeCodeMount || got.HomeDir != "/home/node" {
		t.Errorf("unrelated settings clobbered by automation write: %+v", got)
	}
	if len(got.Agents) != 1 {
		t.Errorf("agent not persisted: %+v", got.Agents)
	}
}

// TestMCPTriggerLogs proves the fleet_trigger_logs tool returns a trigger's
// recorded firings (read from the daemon's on-host log files).
func TestMCPTriggerLogs(t *testing.T) {
	cs := seedBareFleet(t) // isolates the fleet dir (HOME) the logs live under

	// No firings yet → empty, count 0.
	var empty TriggerLogsOutput
	callJSON(t, cs, "fleet_trigger_logs", map[string]any{"fleet": "alpha", "trigger": "nightly"}, &empty)
	if empty.Count != 0 || empty.Logs != "" {
		t.Fatalf("empty history returned %+v", empty)
	}

	// Record a firing, then the tool reports it.
	logTriggerEvent("alpha", &triggerEvent{
		kind:        fleet.TriggerWebhook,
		triggerName: "nightly",
		firedAt:     time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		webhookName: "ci",
		body:        []byte("event-payload"),
	})
	var out TriggerLogsOutput
	callJSON(t, cs, "fleet_trigger_logs", map[string]any{"fleet": "alpha", "trigger": "nightly"}, &out)
	if out.Count != 1 || !strings.Contains(out.Logs, "event-payload") {
		t.Fatalf("trigger logs returned %+v", out)
	}

	// Missing args are a tool error.
	if msg := callErr(t, cs, "fleet_trigger_logs", map[string]any{"fleet": "alpha"}); !strings.Contains(msg, "required") {
		t.Errorf("missing-trigger error = %q", msg)
	}
}
