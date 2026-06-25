package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// automation.go is the shared plumbing for the `fleet agent` and `fleet trigger`
// command trees (issue #189): programmatic CRUD over a fleet's automation
// agents and triggers (the same config the TUI's automation view edits).
//
// There is no per-item RPC — automation lives inline on FleetSettings, which
// SetFleetSettings replaces wholesale. So every write is read-modify-write:
// fetch the fleet's current settings, apply one change to the agent/trigger
// lists (via the shared fleet.* mutators, which own the invariants — unique
// names, rename-rewrites-references, no-orphan deletes), and send the full
// settings back. The other settings fields ride along untouched on the proto
// object we read, so a write here never disturbs mounts, presets, or the rest.
//
// loadAutomation / mutateAutomation are package vars so the command tests can
// stub the server round-trip and exercise the command + mutation logic in
// memory.

// loadAutomation fetches the named fleet's automation lists as domain types.
var loadAutomation = func(ctx context.Context, fleetName string) (fleet.FleetSettings, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	defer conn.Close()
	reply, err := conn.Service().GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return fleet.FleetSettings{}, fmt.Errorf("fleet %q not found", fleetName)
	}
	ps := pf.GetSettings()
	return fleet.FleetSettings{
		Agents:   protoAgentsToFleet(ps.GetAgents()),
		Triggers: protoTriggersToFleet(ps.GetTriggers()),
	}, nil
}

// triggerLogs fetches a trigger's recorded event logs (its recent firings'
// payloads, concatenated) from the daemon. A package var so the command test can
// stub the server round-trip.
var triggerLogs = func(ctx context.Context, fleetName, triggerName string) (string, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	reply, err := conn.Service().TriggerLogs(ctx, &fleetgrpc.TriggerLogsRequest{Fleet: fleetName, Trigger: triggerName})
	if err != nil {
		return "", err
	}
	return reply.GetLogs(), nil
}

// mutateAutomation reads the named fleet's settings, applies fn to its
// (domain) agent/trigger lists, and writes the result back — preserving every
// other settings field. fn returns the modified settings (the shared fleet.*
// mutators do exactly this) or an error that aborts the write.
var mutateAutomation = func(ctx context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) error {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	svc := conn.Service()

	reply, err := svc.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return err
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return fmt.Errorf("fleet %q not found", fleetName)
	}
	// Keep the live proto settings as the carrier so all the fields this command
	// doesn't touch (mounts, presets, home dir, ...) survive the wholesale write.
	ps := pf.GetSettings()
	if ps == nil {
		ps = &fleetgrpc.FleetSettings{}
	}

	settings := fleet.FleetSettings{
		Agents:   protoAgentsToFleet(ps.GetAgents()),
		Triggers: protoTriggersToFleet(ps.GetTriggers()),
	}
	settings, err = fn(settings)
	if err != nil {
		return err
	}
	ps.Agents = fleetAgentsToProto(settings.Agents)
	ps.Triggers = fleetTriggersToProto(settings.Triggers)

	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{Fleet: fleetName, Settings: ps}); err != nil {
		return err
	}
	return nil
}

// --- proto <-> domain converters (agents/triggers only) ---
//
// These mirror the server-side converters (internal/server/convert.go) and the
// TUI's (internal/tui/client.go). The duplication is the depguard boundary's
// price: client code cannot import the server, so each side maps the shared
// proto wire types to its own domain view.

func protoAgentsToFleet(in []*fleetgrpc.Agent) []fleet.Agent {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, fleet.Agent{
			Name:         a.GetName(),
			Command:      a.GetCommand(),
			SystemPrompt: a.GetSystemPrompt(),
			Backend:      fleet.BackendType(backendProtoToString(a.GetBackend())),
		})
	}
	return out
}

func fleetAgentsToProto(in []fleet.Agent) []*fleetgrpc.Agent {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, &fleetgrpc.Agent{
			Name:         a.Name,
			Command:      a.Command,
			SystemPrompt: a.SystemPrompt,
			Backend:      backendTypeToProto(a.Backend),
		})
	}
	return out
}

func protoTriggersToFleet(in []*fleetgrpc.Trigger) []fleet.Trigger {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.Trigger, 0, len(in))
	for _, t := range in {
		out = append(out, fleet.Trigger{
			Name:        t.GetName(),
			Type:        fleet.TriggerType(t.GetType()),
			AgentNames:  t.GetAgentNames(),
			Prompt:      t.GetPrompt(),
			Cron:        t.GetCron(),
			Script:      t.GetScript(),
			WebhookName: t.GetWebhookName(),
			FilterType:  fleet.WebhookFilterType(t.GetFilterType()),
			Regex:       t.GetRegex(),
			JSONPath:    t.GetJsonPath(),
			JSONValue:   t.GetJsonValue(),
		})
	}
	return out
}

func fleetTriggersToProto(in []fleet.Trigger) []*fleetgrpc.Trigger {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.Trigger, 0, len(in))
	for _, t := range in {
		out = append(out, &fleetgrpc.Trigger{
			Name:        t.Name,
			Type:        string(t.Type),
			AgentNames:  t.AgentNames,
			Prompt:      t.Prompt,
			Cron:        t.Cron,
			Script:      t.Script,
			WebhookName: t.WebhookName,
			FilterType:  string(t.FilterType),
			Regex:       t.Regex,
			JsonPath:    t.JSONPath,
			JsonValue:   t.JSONValue,
		})
	}
	return out
}

// backendProtoToString maps the backend enum to the legacy string ("" for
// UNSPECIFIED, which NormalizeAgent then defaults to devcontainer). The reverse,
// backendTypeToProto, lives in jobs.go.
func backendProtoToString(b fleetgrpc.BackendType) string {
	switch b {
	case fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER:
		return string(fleet.BackendDevcontainer)
	case fleetgrpc.BackendType_BACKEND_TYPE_CODER:
		return string(fleet.BackendCoder)
	case fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES:
		return string(fleet.BackendCodespaces)
	default:
		return ""
	}
}
