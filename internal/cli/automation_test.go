package cli

import (
	"reflect"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestAutomationConvertersRoundTrip locks in that the CLI's proto<->domain
// agent/trigger converters are lossless: a write reads the current proto
// settings, converts to domain, mutates, and converts back, so any field that
// doesn't survive the round trip would be silently dropped.
func TestAutomationConvertersRoundTrip(t *testing.T) {
	agents := []fleet.Agent{
		{Name: "a", Command: "claude '${PROMPT}'", SystemPrompt: "be terse", Backend: fleet.BackendCoder},
		{Name: "b", Command: "run", Backend: fleet.BackendDevcontainer},
	}
	if got := protoAgentsToFleet(fleetAgentsToProto(agents)); !reflect.DeepEqual(got, agents) {
		t.Fatalf("agent round trip:\n got %+v\nwant %+v", got, agents)
	}

	triggers := []fleet.Trigger{
		{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a", "b"}, Prompt: "go", Cron: "0 0 * * *"},
		{Name: "hook", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "ci", FilterType: fleet.WebhookFilterJSONPath, JSONPath: "$.action", JSONValue: "opened"},
		{Name: "poll", Type: fleet.TriggerBash, AgentNames: []string{"a"}, Prompt: "drain", Cron: "*/5 * * * *", Script: "test -s /var/queue"},
	}
	if got := protoTriggersToFleet(fleetTriggersToProto(triggers)); !reflect.DeepEqual(got, triggers) {
		t.Fatalf("trigger round trip:\n got %+v\nwant %+v", got, triggers)
	}
}

func TestAutomationConvertersEmpty(t *testing.T) {
	if protoAgentsToFleet(nil) != nil || fleetAgentsToProto(nil) != nil {
		t.Error("empty agent lists should map to nil both ways")
	}
	if protoTriggersToFleet(nil) != nil || fleetTriggersToProto(nil) != nil {
		t.Error("empty trigger lists should map to nil both ways")
	}
}

func TestBackendProtoToString(t *testing.T) {
	// Every backend value survives string -> proto -> string.
	for _, b := range []fleet.BackendType{fleet.BackendDevcontainer, fleet.BackendCoder, fleet.BackendCodespaces} {
		if got := backendProtoToString(backendTypeToProto(b)); got != string(b) {
			t.Errorf("%q round trip: got %q", b, got)
		}
	}
}
