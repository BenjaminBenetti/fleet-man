package fleet

import "testing"

func TestNormalizeAgentDefaults(t *testing.T) {
	a, err := NormalizeAgent(Agent{Name: "  build  "})
	if err != nil {
		t.Fatalf("NormalizeAgent: %v", err)
	}
	if a.Name != "build" {
		t.Errorf("name = %q, want %q", a.Name, "build")
	}
	if a.Command == "" {
		t.Error("command was not defaulted")
	}
	if a.Backend != BackendDevcontainer {
		t.Errorf("backend = %q, want %q", a.Backend, BackendDevcontainer)
	}
}

func TestNormalizeAgentErrors(t *testing.T) {
	if _, err := NormalizeAgent(Agent{Name: "   "}); err == nil {
		t.Error("empty name: want error")
	}
	if _, err := NormalizeAgent(Agent{Name: "x", Backend: "nope"}); err == nil {
		t.Error("invalid backend: want error")
	}
}

func TestNormalizeAgentsDuplicate(t *testing.T) {
	_, err := NormalizeAgents([]Agent{{Name: "a"}, {Name: "a"}})
	if err == nil {
		t.Error("duplicate agent name: want error")
	}
	out, err := NormalizeAgents([]Agent{{Name: "a"}, {Name: "b"}})
	if err != nil {
		t.Fatalf("NormalizeAgents: %v", err)
	}
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Errorf("order not preserved: %+v", out)
	}
}

func TestNormalizeTriggerSchedule(t *testing.T) {
	agents := map[string]struct{}{"build": {}}
	tr, err := NormalizeTrigger(Trigger{
		Name:        "nightly",
		Type:        TriggerSchedule,
		AgentNames:  []string{"build"},
		Cron:        "0 0 * * *",
		WebhookName: "stale", // should be cleared
		Regex:       "stale",
	}, agents)
	if err != nil {
		t.Fatalf("NormalizeTrigger: %v", err)
	}
	if tr.WebhookName != "" || tr.Regex != "" {
		t.Errorf("webhook fields not cleared: %+v", tr)
	}
}

func TestNormalizeTriggerWebhook(t *testing.T) {
	agents := map[string]struct{}{"build": {}}

	// regex filter
	tr, err := NormalizeTrigger(Trigger{
		Name:        "wh",
		Type:        TriggerWebhook,
		AgentNames:  []string{"build"},
		WebhookName: "deploy",
		FilterType:  WebhookFilterRegex,
		Regex:       "push",
		Cron:        "0 0 * * *", // should be cleared
	}, agents)
	if err != nil {
		t.Fatalf("regex webhook: %v", err)
	}
	if tr.Cron != "" {
		t.Errorf("cron not cleared on webhook: %q", tr.Cron)
	}

	// jsonpath filter
	if _, err := NormalizeTrigger(Trigger{
		Name: "wh2", Type: TriggerWebhook, AgentNames: []string{"build"},
		WebhookName: "deploy", FilterType: WebhookFilterJSONPath, JSONPath: "$.action", JSONValue: "opened",
	}, agents); err != nil {
		t.Fatalf("jsonpath webhook: %v", err)
	}

	// bad regex
	if _, err := NormalizeTrigger(Trigger{
		Name: "bad", Type: TriggerWebhook, AgentNames: []string{"build"},
		WebhookName: "x", FilterType: WebhookFilterRegex, Regex: "(",
	}, agents); err == nil {
		t.Error("invalid regex: want error")
	}
}

func TestNormalizeTriggerBash(t *testing.T) {
	agents := map[string]struct{}{"build": {}}
	tr, err := NormalizeTrigger(Trigger{
		Name:        "poll",
		Type:        TriggerBash,
		AgentNames:  []string{"build"},
		Cron:        "*/5 * * * *",
		Script:      "  curl -sf https://example.com/q | grep -q ready  ",
		WebhookName: "stale", // webhook fields should be cleared
		Regex:       "stale",
		JSONPath:    "stale",
	}, agents)
	if err != nil {
		t.Fatalf("NormalizeTrigger bash: %v", err)
	}
	if tr.Script != "curl -sf https://example.com/q | grep -q ready" {
		t.Errorf("script not trimmed/kept: %q", tr.Script)
	}
	if tr.Cron != "*/5 * * * *" {
		t.Errorf("cron not kept: %q", tr.Cron)
	}
	if tr.WebhookName != "" || tr.Regex != "" || tr.JSONPath != "" {
		t.Errorf("webhook fields not cleared: %+v", tr)
	}

	// A schedule / webhook trigger must clear a stray Script (it is bash-only).
	sched, err := NormalizeTrigger(Trigger{
		Name: "s", Type: TriggerSchedule, AgentNames: []string{"build"}, Cron: "0 0 * * *", Script: "echo hi",
	}, agents)
	if err != nil {
		t.Fatalf("NormalizeTrigger schedule: %v", err)
	}
	if sched.Script != "" {
		t.Errorf("schedule trigger kept a script: %q", sched.Script)
	}
	wh, err := NormalizeTrigger(Trigger{
		Name: "w", Type: TriggerWebhook, AgentNames: []string{"build"},
		WebhookName: "deploy", FilterType: WebhookFilterRegex, Regex: "push", Script: "echo hi",
	}, agents)
	if err != nil {
		t.Fatalf("NormalizeTrigger webhook: %v", err)
	}
	if wh.Script != "" {
		t.Errorf("webhook trigger kept a script: %q", wh.Script)
	}
}

func TestTriggerTypeIsCron(t *testing.T) {
	for _, c := range []struct {
		typ  TriggerType
		want bool
	}{
		{TriggerSchedule, true},
		{TriggerBash, true},
		{TriggerWebhook, false},
		{"bogus", false},
	} {
		if got := c.typ.IsCron(); got != c.want {
			t.Errorf("%q.IsCron() = %v, want %v", c.typ, got, c.want)
		}
	}
}

func TestNormalizeTriggerErrors(t *testing.T) {
	agents := map[string]struct{}{"build": {}}
	bad := []Trigger{
		{Name: "", Type: TriggerSchedule, AgentNames: []string{"build"}, Cron: "* * * * *"},                                                       // empty name
		{Name: "x", Type: TriggerSchedule, AgentNames: nil, Cron: "* * * * *"},                                                                    // no agents
		{Name: "x", Type: TriggerSchedule, AgentNames: []string{"ghost"}, Cron: "* * * * *"},                                                      // unknown agent
		{Name: "x", Type: TriggerSchedule, AgentNames: []string{"build"}, Cron: "nope"},                                                           // bad cron
		{Name: "x", Type: "bogus", AgentNames: []string{"build"}},                                                                                 // bad type
		{Name: "x", Type: TriggerWebhook, AgentNames: []string{"build"}, WebhookName: ""},                                                         // empty webhook name
		{Name: "x", Type: TriggerWebhook, AgentNames: []string{"build"}, WebhookName: "w"},                                                        // missing filter type
		{Name: "x", Type: TriggerWebhook, AgentNames: []string{"build"}, WebhookName: "w", FilterType: WebhookFilterRegex, Regex: "("},            // bad regex
		{Name: "x", Type: TriggerWebhook, AgentNames: []string{"build"}, WebhookName: "w", FilterType: WebhookFilterJSONPath, JSONPath: "$.a..b"}, // malformed json path
		{Name: "x", Type: TriggerBash, AgentNames: []string{"build"}, Cron: "* * * * *", Script: ""},                                              // bash: empty script
		{Name: "x", Type: TriggerBash, AgentNames: []string{"build"}, Cron: "nope", Script: "true"},                                               // bash: bad cron
	}
	for i, tr := range bad {
		if _, err := NormalizeTrigger(tr, agents); err == nil {
			t.Errorf("case %d: want error, got nil for %+v", i, tr)
		}
	}
}

func TestNormalizeTriggersDuplicateAndRefs(t *testing.T) {
	agents := []Agent{{Name: "build", Backend: BackendDevcontainer}}
	_, err := NormalizeTriggers([]Trigger{
		{Name: "a", Type: TriggerSchedule, AgentNames: []string{"build"}, Cron: "* * * * *"},
		{Name: "a", Type: TriggerSchedule, AgentNames: []string{"build"}, Cron: "* * * * *"},
	}, agents)
	if err == nil {
		t.Error("duplicate trigger name: want error")
	}
}

func TestSubstituteAgentCommand(t *testing.T) {
	// Values are single-quote-escaped; placeholders are wrapped in single quotes
	// (as the default command is).
	got := SubstituteAgentCommand(`claude --system-prompt '${SYS_PROMPT}' '${PROMPT}'`, "do the thing", "be helpful")
	want := `claude --system-prompt 'be helpful' 'do the thing'`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A ${PROMPT} that appears inside the system prompt text must not be
	// re-expanded by the prompt pass.
	got = SubstituteAgentCommand(`x '${SYS_PROMPT}' '${PROMPT}'`, "P", "has ${PROMPT}")
	want = `x 'has ${PROMPT}' 'P'`
	if got != want {
		t.Errorf("re-expansion guard: got %q, want %q", got, want)
	}

	// A single quote in the value is escaped so it can't break out of the
	// surrounding single quotes (and shell metacharacters stay literal).
	got = SubstituteAgentCommand(`claude '${PROMPT}'`, "it's; rm -rf /", "")
	want = `claude 'it'\''s; rm -rf /'`
	if got != want {
		t.Errorf("quote-escape: got %q, want %q", got, want)
	}
}
