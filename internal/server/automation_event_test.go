package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestTriggerEventPayload(t *testing.T) {
	now := time.Date(2026, 6, 22, 9, 30, 15, 0, time.UTC)

	// Webhook: the raw request body is the payload, verbatim.
	body := []byte(`{"action":"opened","number":7}`)
	web := &triggerEvent{kind: fleet.TriggerWebhook, body: body, firedAt: now}
	if got := web.payload(); !bytes.Equal(got, body) {
		t.Fatalf("webhook payload = %q, want %q", got, body)
	}

	// Schedule: no external payload, so the fire time stands in.
	sch := &triggerEvent{kind: fleet.TriggerSchedule, firedAt: now}
	if got, want := string(sch.payload()), "2026-06-22T09:30:15Z\n"; got != want {
		t.Fatalf("schedule payload = %q, want %q", got, want)
	}
}

func TestAppendEventPrompt(t *testing.T) {
	path := "/tmp/fleet-event-20260622T093015Z"

	web := &triggerEvent{kind: fleet.TriggerWebhook, triggerName: "ci", webhookName: "ci"}
	got := appendEventPrompt("fix the build", web, path)
	for _, want := range []string{"fix the build", "\n\n---\n", `"ci" webhook trigger`, path, "read that file"} {
		if !strings.Contains(got, want) {
			t.Fatalf("webhook prompt missing %q:\n%s", want, got)
		}
	}

	sch := &triggerEvent{kind: fleet.TriggerSchedule, triggerName: "nightly"}
	got = appendEventPrompt("run the report", sch, path)
	for _, want := range []string{"run the report", `"nightly" schedule trigger`, path} {
		if !strings.Contains(got, want) {
			t.Fatalf("schedule prompt missing %q:\n%s", want, got)
		}
	}

	// A blank prompt becomes just the note (no leading separator/whitespace).
	got = appendEventPrompt("   ", web, path)
	if strings.HasPrefix(got, " ") || strings.Contains(got, "---") {
		t.Fatalf("blank prompt should yield just the note, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "This session was started automatically") {
		t.Fatalf("blank prompt note unexpected start:\n%s", got)
	}
}

func TestAutomationEventPath(t *testing.T) {
	// UTC, fixed prefix, second-granular stamp — stable and collision-free within
	// a single (fresh-per-run) instance.
	got := automationEventPath(time.Date(2026, 6, 22, 9, 30, 15, 0, time.FixedZone("x", 3600)))
	if want := "/tmp/fleet-event-20260622T083015Z"; got != want {
		t.Fatalf("event path = %q, want %q", got, want)
	}
}

// TestWriteAutomationEventFile drives the real base64-over-stdin write by stubbing
// the copy exec seam to run the same argv locally — so a payload of arbitrary
// bytes round-trips through `base64 -d` exactly as it would inside a container.
func TestWriteAutomationEventFile(t *testing.T) {
	dir := t.TempDir()
	orig := copyIntoExecCommand
	copyIntoExecCommand = func(_ *fleet.Instance, argv []string) *exec.Cmd {
		return exec.Command(argv[0], argv[1:]...)
	}
	t.Cleanup(func() { copyIntoExecCommand = orig })

	cases := map[string][]byte{
		"binary": {0x00, 0x01, 'h', 'i', 0xff, '\n', 0x04},
		"json":   []byte(`{"a":1,"b":"two"}` + "\n"),
		"empty":  {},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := writeAutomationEventFile(&fleet.Instance{}, path, data); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("round-trip mismatch: got %q want %q", got, data)
			}
		})
	}
}

// TestFireTriggerAgentsAttachesEvent confirms both fire paths hang the right
// triggerEvent off each watched agent: a webhook carries its body, a schedule
// carries none (its payload is synthesized from the fire time at launch).
func TestFireTriggerAgentsAttachesEvent(t *testing.T) {
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents: []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
		}},
	}}
	origLoad := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	origCreate := createAutomationInstance
	createAutomationInstance = func(_ *service, _ string, ag fleet.Agent, _ time.Time) (string, error) {
		return "inst-" + ag.Name, nil
	}
	t.Cleanup(func() { scheduleLoadState = origLoad; createAutomationInstance = origCreate })

	s := &service{}
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)

	// Webhook path: body rides through to the event.
	body := []byte(`{"ref":"main"}`)
	sched := newScheduler()
	wtrig := fleet.Trigger{Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "ci"}
	s.fireWebhookBatch(sched, []webhookFire{{fleet: "alpha", trigger: wtrig, body: body}}, now)
	ev := sched.watched["alpha/inst-a"].event
	if ev == nil {
		t.Fatal("webhook agent has no event")
	}
	if ev.kind != fleet.TriggerWebhook || ev.triggerName != "ci" || ev.webhookName != "ci" {
		t.Fatalf("webhook event fields wrong: %+v", ev)
	}
	if !bytes.Equal(ev.body, body) {
		t.Fatalf("webhook event body = %q, want %q", ev.body, body)
	}

	// Schedule path: nil body, kind=schedule, fire time recorded.
	sched = newScheduler()
	strig := fleet.Trigger{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}}
	s.fireTriggerAgents(sched, "alpha", strig, []fleet.Agent{{Name: "a"}}, now, nil)
	ev = sched.watched["alpha/inst-a"].event
	if ev == nil {
		t.Fatal("schedule agent has no event")
	}
	if ev.kind != fleet.TriggerSchedule || ev.body != nil || !ev.firedAt.Equal(now) {
		t.Fatalf("schedule event fields wrong: %+v", ev)
	}
}

// TestServeWebhookCarriesBodyToFire confirms the request body survives the whole
// receiver→scheduler hand-off, so the agent really gets the event it fired on.
func TestServeWebhookCarriesBodyToFire(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "deploy", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "deploy", FilterType: fleet.WebhookFilterRegex, Regex: `"env":"prod"`,
	})
	payload := `{"env":"prod","sha":"abc123"}`
	if w := postWebhook(t, s, st, "deploy", payload); w.Code != 200 {
		t.Fatalf("status = %d, body=%q", w.Code, w.Body.String())
	}
	batch := drainBatch(s)
	if len(batch) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(batch))
	}
	if string(batch[0].body) != payload {
		t.Fatalf("fire body = %q, want %q", batch[0].body, payload)
	}
}
