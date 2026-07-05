package server

import (
	"reflect"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestInstanceAutomatedFlagToProto guards the automation-origin marker (issue
// #188) across the instance wire mapping: a scheduler-spawned instance must
// carry Automated=true to the TUI, and a user-created one must stay false.
func TestInstanceAutomatedFlagToProto(t *testing.T) {
	if !instanceToProto(&fleet.Instance{Name: "x", Automated: true}).GetAutomated() {
		t.Fatal("instanceToProto should carry Automated=true")
	}
	if instanceToProto(&fleet.Instance{Name: "x"}).GetAutomated() {
		t.Fatal("a non-automated instance should map to automated=false")
	}
}

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
			{
				Name:       "poll-queue",
				Type:       fleet.TriggerBash,
				AgentNames: []string{"builder"},
				Prompt:     "drain it",
				Cron:       "*/5 * * * *",
				Script:     "test -s /var/queue",
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

// TestFleetSettingsCoderRoundTrip guards the per-fleet coder settings wire
// mapping (issue #221): template, preset, the workspace-name override, and the
// RICH parameter list (value + template metadata) must survive
// legacy -> proto -> legacy unchanged, so a SetFleetSettings round-trip never
// silently drops a field.
func TestFleetSettingsCoderRoundTrip(t *testing.T) {
	in := fleet.FleetSettings{
		CoderTemplate:      "k8s-devbox",
		CoderPreset:        "large",
		CoderWorkspaceName: "myproj",
		CoderParameters: []fleet.CoderParameter{
			{Name: "repo", Value: "${GIT_URL}", DefaultValue: "d1", DisplayName: "Repo URL", Description: "clone target", Type: "string"},
			{Name: "cpus", Value: "4"},
		},
	}

	out := protoFleetSettingsToLegacy(fleetSettingsToProto(in))

	if !reflect.DeepEqual(in, out) {
		t.Errorf("coder settings round-trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}
