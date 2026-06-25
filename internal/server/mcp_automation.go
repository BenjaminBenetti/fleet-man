package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_automation.go adds the automation CRUD tools (issue #189): create / read /
// update / delete for a fleet's automation agents and triggers — the same
// config the TUI's automation view and the `fleet agent` / `fleet trigger` CLI
// edit. They let the fleet admiral set up automations programmatically.
//
// Automation lives inline on FleetSettings, which SetFleetSettings replaces
// wholesale, so every write is read-modify-write through mutateAutomation:
// fetch the fleet's full settings, apply one change via the shared fleet.*
// mutators (which own the invariants — unique names, rename-rewrites-references,
// no-orphan deletes — and re-validate, matching what SetFleetSettings enforces
// authoritatively), and write the full settings back. Every write returns the
// fleet's resulting automation config so the caller sees the new state.

// --- output shapes ---

// mcpAgent / mcpTrigger are the JSON-friendly views of an automation agent /
// trigger.
type mcpAgent struct {
	Name         string `json:"name"`
	Command      string `json:"command,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Backend      string `json:"backend,omitempty"`
}

type mcpTrigger struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Agents      []string `json:"agents"`
	Prompt      string   `json:"prompt,omitempty"`
	Cron        string   `json:"cron,omitempty"`
	Script      string   `json:"script,omitempty"`
	WebhookName string   `json:"webhook_name,omitempty"`
	FilterType  string   `json:"filter_type,omitempty"`
	Regex       string   `json:"regex,omitempty"`
	JSONPath    string   `json:"json_path,omitempty"`
	JSONValue   string   `json:"json_value,omitempty"`
}

// AutomationOutput is a fleet's full automation config, returned by the read
// tool and by every write tool (so the caller sees the result of its change).
type AutomationOutput struct {
	Agents   []mcpAgent   `json:"agents"`
	Triggers []mcpTrigger `json:"triggers"`
}

func toMCPAutomation(s fleet.FleetSettings) AutomationOutput {
	out := AutomationOutput{Agents: []mcpAgent{}, Triggers: []mcpTrigger{}}
	for _, a := range s.Agents {
		out.Agents = append(out.Agents, mcpAgent{
			Name:         a.Name,
			Command:      a.Command,
			SystemPrompt: a.SystemPrompt,
			Backend:      string(a.Backend),
		})
	}
	for _, t := range s.Triggers {
		out.Triggers = append(out.Triggers, mcpTrigger{
			Name:        t.Name,
			Type:        string(t.Type),
			Agents:      t.AgentNames,
			Prompt:      t.Prompt,
			Cron:        t.Cron,
			Script:      t.Script,
			WebhookName: t.WebhookName,
			FilterType:  string(t.FilterType),
			Regex:       t.Regex,
			JSONPath:    t.JSONPath,
			JSONValue:   t.JSONValue,
		})
	}
	return out
}

// --- shared read-modify-write ---

// automationSettings reads the named fleet's full settings (NotFound if the
// fleet is unknown).
func (s *service) automationSettings(ctx context.Context, fleetName string) (fleet.FleetSettings, error) {
	reply, err := s.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return fleet.FleetSettings{}, mcpErr(err)
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return fleet.FleetSettings{}, fmt.Errorf("fleet %q not found", fleetName)
	}
	return protoFleetSettingsToLegacy(pf.GetSettings()), nil
}

// mutateAutomation reads the fleet's full settings, applies fn to them, and
// writes the result back — preserving every settings field fn doesn't touch.
// It returns the settings as persisted.
func (s *service) mutateAutomation(ctx context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) (fleet.FleetSettings, error) {
	settings, err := s.automationSettings(ctx, fleetName)
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	settings, err = fn(settings)
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	if _, err := s.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{Fleet: fleetName, Settings: fleetSettingsToProto(settings)}); err != nil {
		return fleet.FleetSettings{}, mcpErr(err)
	}
	return settings, nil
}

// --- read ---

type FleetAutomationListInput struct {
	Fleet string `json:"fleet" jsonschema:"fleet name"`
}

func (s *service) mcpAutomationList(ctx context.Context, _ *mcp.CallToolRequest, in FleetAutomationListInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" {
		return nil, AutomationOutput{}, errors.New("fleet is required")
	}
	settings, err := s.automationSettings(ctx, in.Fleet)
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

// --- agents ---

type FleetAgentCreateInput struct {
	Fleet        string `json:"fleet" jsonschema:"fleet name"`
	Name         string `json:"name" jsonschema:"agent name, unique within the fleet"`
	Command      string `json:"command,omitempty" jsonschema:"launch command; ${PROMPT} and ${SYS_PROMPT} are substituted when a trigger fires it. Defaults to a live claude session command if omitted"`
	SystemPrompt string `json:"system_prompt,omitempty" jsonschema:"system prompt injected into the command's ${SYS_PROMPT}"`
	Backend      string `json:"backend,omitempty" jsonschema:"env backend the agent's instance runs on: devcontainer (default), coder, or codespaces"`
}

func (s *service) mcpAgentCreate(ctx context.Context, _ *mcp.CallToolRequest, in FleetAgentCreateInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" || in.Name == "" {
		return nil, AutomationOutput{}, errors.New("fleet and name are required")
	}
	settings, err := s.mutateAutomation(ctx, in.Fleet, func(st fleet.FleetSettings) (fleet.FleetSettings, error) {
		return fleet.AddAgent(st, fleet.Agent{
			Name:         in.Name,
			Command:      in.Command,
			SystemPrompt: in.SystemPrompt,
			Backend:      fleet.BackendType(in.Backend),
		})
	})
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

type FleetAgentUpdateInput struct {
	Fleet        string `json:"fleet" jsonschema:"fleet name"`
	Name         string `json:"name" jsonschema:"name of the agent to update"`
	NewName      string `json:"new_name,omitempty" jsonschema:"rename the agent (also rewrites triggers that reference it); omit to keep the name"`
	Command      string `json:"command,omitempty" jsonschema:"new launch command; omit to keep the current one"`
	SystemPrompt string `json:"system_prompt,omitempty" jsonschema:"new system prompt; omit to keep the current one"`
	Backend      string `json:"backend,omitempty" jsonschema:"new backend (devcontainer, coder, codespaces); omit to keep the current one"`
}

func (s *service) mcpAgentUpdate(ctx context.Context, _ *mcp.CallToolRequest, in FleetAgentUpdateInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" || in.Name == "" {
		return nil, AutomationOutput{}, errors.New("fleet and name are required")
	}
	settings, err := s.mutateAutomation(ctx, in.Fleet, func(st fleet.FleetSettings) (fleet.FleetSettings, error) {
		a, ok := fleet.FindAgent(st.Agents, in.Name)
		if !ok {
			return st, fmt.Errorf("agent %q not found", in.Name)
		}
		// Empty means "keep current" — JSON value-typed fields can't tell an
		// omitted field from an explicit empty one, so an update is a merge.
		if in.NewName != "" {
			a.Name = in.NewName
		}
		if in.Command != "" {
			a.Command = in.Command
		}
		if in.SystemPrompt != "" {
			a.SystemPrompt = in.SystemPrompt
		}
		if in.Backend != "" {
			a.Backend = fleet.BackendType(in.Backend)
		}
		return fleet.UpdateAgent(st, in.Name, a)
	})
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

// FleetAutomationItemInput identifies one automation item (agent or trigger) by
// name for the delete tools.
type FleetAutomationItemInput struct {
	Fleet string `json:"fleet" jsonschema:"fleet name"`
	Name  string `json:"name" jsonschema:"name of the agent or trigger"`
}

func (s *service) mcpAgentDelete(ctx context.Context, _ *mcp.CallToolRequest, in FleetAutomationItemInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" || in.Name == "" {
		return nil, AutomationOutput{}, errors.New("fleet and name are required")
	}
	settings, err := s.mutateAutomation(ctx, in.Fleet, func(st fleet.FleetSettings) (fleet.FleetSettings, error) {
		return fleet.DeleteAgent(st, in.Name)
	})
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

// --- triggers ---

type FleetTriggerCreateInput struct {
	Fleet       string   `json:"fleet" jsonschema:"fleet name"`
	Name        string   `json:"name" jsonschema:"trigger name, unique within the fleet"`
	Type        string   `json:"type" jsonschema:"trigger type: schedule (cron), webhook, or bash (cron-polled command)"`
	Agents      []string `json:"agents" jsonschema:"names of the agents this trigger activates (at least one, each must exist in the fleet)"`
	Prompt      string   `json:"prompt,omitempty" jsonschema:"prompt fed to the agents via ${PROMPT}"`
	Cron        string   `json:"cron,omitempty" jsonschema:"schedule and bash types: 5-field cron expression, e.g. 0 9 * * 1-5"`
	Script      string   `json:"script,omitempty" jsonschema:"bash type: command run on the fleet host each time the cron is due; a zero exit fires the agents and the command's stdout is the event payload"`
	WebhookName string   `json:"webhook_name,omitempty" jsonschema:"webhook type: name appended to the gateway webhook URL"`
	FilterType  string   `json:"filter_type,omitempty" jsonschema:"webhook type: how events are matched, regex or jsonpath"`
	Regex       string   `json:"regex,omitempty" jsonschema:"webhook regex filter: the event body must match this expression"`
	JSONPath    string   `json:"json_path,omitempty" jsonschema:"webhook jsonpath filter: the JSON path selected from the event"`
	JSONValue   string   `json:"json_value,omitempty" jsonschema:"webhook jsonpath filter: the value the selected path must equal"`
}

func triggerFromCreate(in FleetTriggerCreateInput) fleet.Trigger {
	return fleet.Trigger{
		Name:        in.Name,
		Type:        fleet.TriggerType(in.Type),
		AgentNames:  in.Agents,
		Prompt:      in.Prompt,
		Cron:        in.Cron,
		Script:      in.Script,
		WebhookName: in.WebhookName,
		FilterType:  fleet.WebhookFilterType(in.FilterType),
		Regex:       in.Regex,
		JSONPath:    in.JSONPath,
		JSONValue:   in.JSONValue,
	}
}

func (s *service) mcpTriggerCreate(ctx context.Context, _ *mcp.CallToolRequest, in FleetTriggerCreateInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" || in.Name == "" {
		return nil, AutomationOutput{}, errors.New("fleet and name are required")
	}
	settings, err := s.mutateAutomation(ctx, in.Fleet, func(st fleet.FleetSettings) (fleet.FleetSettings, error) {
		return fleet.AddTrigger(st, triggerFromCreate(in))
	})
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

type FleetTriggerUpdateInput struct {
	Fleet       string   `json:"fleet" jsonschema:"fleet name"`
	Name        string   `json:"name" jsonschema:"name of the trigger to update"`
	NewName     string   `json:"new_name,omitempty" jsonschema:"rename the trigger; omit to keep the name"`
	Type        string   `json:"type,omitempty" jsonschema:"new type, schedule, webhook, or bash; omit to keep current"`
	Agents      []string `json:"agents,omitempty" jsonschema:"replacement set of agent names; omit to keep current"`
	Prompt      string   `json:"prompt,omitempty" jsonschema:"new prompt; omit to keep current"`
	Cron        string   `json:"cron,omitempty" jsonschema:"new cron expression; omit to keep current"`
	Script      string   `json:"script,omitempty" jsonschema:"new bash command (bash type); omit to keep current"`
	WebhookName string   `json:"webhook_name,omitempty" jsonschema:"new webhook name; omit to keep current"`
	FilterType  string   `json:"filter_type,omitempty" jsonschema:"new webhook filter type, regex or jsonpath; omit to keep current"`
	Regex       string   `json:"regex,omitempty" jsonschema:"new webhook regex; omit to keep current"`
	JSONPath    string   `json:"json_path,omitempty" jsonschema:"new webhook JSON path; omit to keep current"`
	JSONValue   string   `json:"json_value,omitempty" jsonschema:"new webhook JSON value; omit to keep current"`
}

func (s *service) mcpTriggerUpdate(ctx context.Context, _ *mcp.CallToolRequest, in FleetTriggerUpdateInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" || in.Name == "" {
		return nil, AutomationOutput{}, errors.New("fleet and name are required")
	}
	settings, err := s.mutateAutomation(ctx, in.Fleet, func(st fleet.FleetSettings) (fleet.FleetSettings, error) {
		t, ok := fleet.FindTrigger(st.Triggers, in.Name)
		if !ok {
			return st, fmt.Errorf("trigger %q not found", in.Name)
		}
		// Empty means "keep current" (see mcpAgentUpdate).
		if in.NewName != "" {
			t.Name = in.NewName
		}
		if in.Type != "" {
			t.Type = fleet.TriggerType(in.Type)
		}
		if len(in.Agents) > 0 {
			t.AgentNames = in.Agents
		}
		if in.Prompt != "" {
			t.Prompt = in.Prompt
		}
		if in.Cron != "" {
			t.Cron = in.Cron
		}
		if in.Script != "" {
			t.Script = in.Script
		}
		if in.WebhookName != "" {
			t.WebhookName = in.WebhookName
		}
		if in.FilterType != "" {
			t.FilterType = fleet.WebhookFilterType(in.FilterType)
		}
		if in.Regex != "" {
			t.Regex = in.Regex
		}
		if in.JSONPath != "" {
			t.JSONPath = in.JSONPath
		}
		if in.JSONValue != "" {
			t.JSONValue = in.JSONValue
		}
		return fleet.UpdateTrigger(st, in.Name, t)
	})
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

func (s *service) mcpTriggerDelete(ctx context.Context, _ *mcp.CallToolRequest, in FleetAutomationItemInput) (*mcp.CallToolResult, AutomationOutput, error) {
	if in.Fleet == "" || in.Name == "" {
		return nil, AutomationOutput{}, errors.New("fleet and name are required")
	}
	settings, err := s.mutateAutomation(ctx, in.Fleet, func(st fleet.FleetSettings) (fleet.FleetSettings, error) {
		return fleet.DeleteTrigger(st, in.Name)
	})
	if err != nil {
		return nil, AutomationOutput{}, err
	}
	return nil, toMCPAutomation(settings), nil
}

// --- trigger event logs ---

type FleetTriggerLogsInput struct {
	Fleet   string `json:"fleet" jsonschema:"fleet name"`
	Trigger string `json:"trigger" jsonschema:"trigger name"`
}

// TriggerLogsOutput is a trigger's recorded event logs, concatenated for reading.
type TriggerLogsOutput struct {
	// Logs is every recorded firing's payload concatenated (oldest first), each
	// preceded by a separator header; empty when the trigger has fired nothing.
	Logs string `json:"logs"`
	// Count is how many event logs were concatenated.
	Count int `json:"count"`
}

func (s *service) mcpTriggerLogs(_ context.Context, _ *mcp.CallToolRequest, in FleetTriggerLogsInput) (*mcp.CallToolResult, TriggerLogsOutput, error) {
	if in.Fleet == "" || in.Trigger == "" {
		return nil, TriggerLogsOutput{}, errors.New("fleet and trigger are required")
	}
	logs, count, err := readTriggerLogs(in.Fleet, in.Trigger)
	if err != nil {
		return nil, TriggerLogsOutput{}, mcpErr(err)
	}
	return nil, TriggerLogsOutput{Logs: logs, Count: count}, nil
}
