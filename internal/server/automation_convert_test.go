package server

import (
	"reflect"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestFleetSettingsAutomationRoundTrip guards the agents/triggers wire mapping:
// a settings value carrying automation agents + triggers must survive
// legacy -> proto -> legacy unchanged, so a SetFleetSettings round-trip never
// silently drops a field.
func TestFleetSettingsAutomationRoundTrip(t *testing.T) {
	in := fleet.FleetSettings{
		Agents: []fleet.Agent{
			{
				Name:         "builder",
				Command:      "claude --system-prompt '${SYS_PROMPT}' '${PROMPT}'",
				SystemPrompt: "be precise",
				Backend:      fleet.BackendDevcontainer,
			},
			{
				Name:    "noTmux",
				Command: "echo hi",
				Backend: fleet.BackendCoder,
			},
		},
		Triggers: []fleet.Trigger{
			{
				Name:       "nightly",
				Type:       fleet.TriggerSchedule,
				AgentNames: []string{"builder"},
				Prompt:     "rebuild the world",
				Cron:       "0 0 * * *",
			},
			{
				Name:        "on-push",
				Type:        fleet.TriggerWebhook,
				AgentNames:  []string{"builder", "noTmux"},
				Prompt:      "react",
				WebhookName: "push",
				FilterType:  fleet.WebhookFilterJSONPath,
				JSONPath:    "$.ref",
				JSONValue:   "refs/heads/main",
				Disabled:    true,
			},
		},
	}

	out := protoFleetSettingsToLegacy(fleetSettingsToProto(in))

	if !reflect.DeepEqual(in.Agents, out.Agents) {
		t.Errorf("agents round-trip mismatch:\n in: %+v\nout: %+v", in.Agents, out.Agents)
	}
	if !reflect.DeepEqual(in.Triggers, out.Triggers) {
		t.Errorf("triggers round-trip mismatch:\n in: %+v\nout: %+v", in.Triggers, out.Triggers)
	}
}
