package fleet

import (
	"slices"
	"strings"
	"testing"
)

// settingsWith builds a FleetSettings carrying just the automation lists (the
// only fields the mutation helpers touch).
func settingsWith(agents []Agent, triggers []Trigger) FleetSettings {
	return FleetSettings{Agents: agents, Triggers: triggers}
}

func TestAddAgent(t *testing.T) {
	s, err := AddAgent(FleetSettings{}, Agent{Name: "  builder  "})
	if err != nil {
		t.Fatalf("AddAgent: %v", err)
	}
	if len(s.Agents) != 1 {
		t.Fatalf("want 1 agent, got %d", len(s.Agents))
	}
	a := s.Agents[0]
	if a.Name != "builder" {
		t.Errorf("name = %q, want trimmed %q", a.Name, "builder")
	}
	// An empty command normalizes to the default; backend defaults to devcontainer.
	if a.Command != strings.TrimSpace(DefaultAgentCommand) {
		t.Errorf("command = %q, want default", a.Command)
	}
	if a.Backend != BackendDevcontainer {
		t.Errorf("backend = %q, want devcontainer", a.Backend)
	}
}

func TestAddAgentRejectsDuplicate(t *testing.T) {
	s := settingsWith([]Agent{{Name: "a", Backend: BackendDevcontainer}}, nil)
	if _, err := AddAgent(s, Agent{Name: "a"}); err == nil {
		t.Fatal("want duplicate-name error")
	}
}

func TestAddAgentRejectsEmptyName(t *testing.T) {
	if _, err := AddAgent(FleetSettings{}, Agent{Name: "   "}); err == nil {
		t.Fatal("want empty-name error")
	}
}

func TestAddAgentDoesNotMutateInput(t *testing.T) {
	orig := []Agent{{Name: "a", Backend: BackendDevcontainer}}
	s := settingsWith(orig, nil)
	if _, err := AddAgent(s, Agent{Name: "b"}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}
	if len(orig) != 1 {
		t.Fatalf("input slice was mutated: len=%d", len(orig))
	}
}

func TestUpdateAgentRenameRewritesTriggerRefs(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "old", Backend: BackendDevcontainer}},
		[]Trigger{{Name: "t", Type: TriggerSchedule, AgentNames: []string{"old"}, Cron: "* * * * *"}},
	)
	out, err := UpdateAgent(s, "old", Agent{Name: "new", Backend: BackendDevcontainer})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if out.Agents[0].Name != "new" {
		t.Errorf("agent not renamed: %+v", out.Agents)
	}
	if got := out.Triggers[0].AgentNames; len(got) != 1 || got[0] != "new" {
		t.Errorf("trigger ref not rewritten on rename: %v", got)
	}
	// The original settings must be untouched (optimistic-revert safety).
	if s.Agents[0].Name != "old" || s.Triggers[0].AgentNames[0] != "old" {
		t.Errorf("UpdateAgent mutated its input: %+v / %+v", s.Agents, s.Triggers)
	}
}

func TestUpdateAgentRenameRewritesAllTriggers(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "old", Backend: BackendDevcontainer}},
		[]Trigger{
			{Name: "t1", Type: TriggerSchedule, AgentNames: []string{"old"}, Cron: "* * * * *"},
			{Name: "t2", Type: TriggerSchedule, AgentNames: []string{"old"}, Cron: "0 0 * * *"},
			{Name: "t3", Type: TriggerWebhook, AgentNames: []string{"old"}, WebhookName: "ci", FilterType: WebhookFilterRegex, Regex: "push"},
		},
	)
	out, err := UpdateAgent(s, "old", Agent{Name: "new", Backend: BackendDevcontainer})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	for i, tr := range out.Triggers {
		if !slices.Equal(tr.AgentNames, []string{"new"}) {
			t.Errorf("trigger %d (%q) ref not rewritten: %v", i, tr.Name, tr.AgentNames)
		}
	}
}

func TestUpdateAgentMissing(t *testing.T) {
	if _, err := UpdateAgent(FleetSettings{}, "ghost", Agent{Name: "ghost"}); err == nil {
		t.Fatal("want not-found error")
	}
}

func TestUpdateAgentRenameCollision(t *testing.T) {
	s := settingsWith([]Agent{
		{Name: "a", Backend: BackendDevcontainer},
		{Name: "b", Backend: BackendDevcontainer},
	}, nil)
	// Renaming "a" to "b" collides with the existing "b".
	if _, err := UpdateAgent(s, "a", Agent{Name: "b", Backend: BackendDevcontainer}); err == nil {
		t.Fatal("want collision error")
	}
}

func TestUpdateAgentInPlaceNoRename(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "a", Backend: BackendDevcontainer, Command: "old"}},
		[]Trigger{{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	)
	out, err := UpdateAgent(s, "a", Agent{Name: "a", Backend: BackendCoder, Command: "new"})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if out.Agents[0].Command != "new" || out.Agents[0].Backend != BackendCoder {
		t.Errorf("fields not updated: %+v", out.Agents[0])
	}
	// No rename, so the trigger reference is unchanged and still valid.
	if got := out.Triggers[0].AgentNames; len(got) != 1 || got[0] != "a" {
		t.Errorf("trigger ref changed unexpectedly: %v", got)
	}
}

func TestDeleteAgentBlockedWhenReferenced(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "a", Backend: BackendDevcontainer}},
		[]Trigger{{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	)
	if _, err := DeleteAgent(s, "a"); err == nil {
		t.Fatal("want referenced-agent error")
	} else if !strings.Contains(err.Error(), "trigger \"t\"") {
		t.Errorf("error should name the referencing trigger: %v", err)
	}
}

func TestDeleteAgentUnreferenced(t *testing.T) {
	s := settingsWith([]Agent{
		{Name: "a", Backend: BackendDevcontainer},
		{Name: "b", Backend: BackendDevcontainer},
	}, nil)
	out, err := DeleteAgent(s, "a")
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if len(out.Agents) != 1 || out.Agents[0].Name != "b" {
		t.Errorf("want [b] left, got %+v", out.Agents)
	}
	if len(s.Agents) != 2 {
		t.Errorf("DeleteAgent mutated its input: %+v", s.Agents)
	}
}

func TestDeleteAgentMissing(t *testing.T) {
	if _, err := DeleteAgent(FleetSettings{}, "ghost"); err == nil {
		t.Fatal("want not-found error")
	}
}

func TestAddTrigger(t *testing.T) {
	s := settingsWith([]Agent{{Name: "a", Backend: BackendDevcontainer}}, nil)
	out, err := AddTrigger(s, Trigger{Name: "nightly", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 0 * * *"})
	if err != nil {
		t.Fatalf("AddTrigger: %v", err)
	}
	if len(out.Triggers) != 1 || out.Triggers[0].Name != "nightly" {
		t.Fatalf("trigger not added: %+v", out.Triggers)
	}
}

func TestAddTriggerUnknownAgent(t *testing.T) {
	s := settingsWith([]Agent{{Name: "a", Backend: BackendDevcontainer}}, nil)
	if _, err := AddTrigger(s, Trigger{Name: "t", Type: TriggerSchedule, AgentNames: []string{"ghost"}, Cron: "* * * * *"}); err == nil {
		t.Fatal("want unknown-agent error")
	}
}

func TestAddTriggerBadCron(t *testing.T) {
	s := settingsWith([]Agent{{Name: "a", Backend: BackendDevcontainer}}, nil)
	if _, err := AddTrigger(s, Trigger{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "nope"}); err == nil {
		t.Fatal("want invalid-cron error")
	}
}

func TestAddTriggerRejectsDuplicate(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "a", Backend: BackendDevcontainer}},
		[]Trigger{{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	)
	if _, err := AddTrigger(s, Trigger{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}); err == nil {
		t.Fatal("want duplicate-name error")
	}
}

func TestAddTriggerWebhookClearsScheduleFields(t *testing.T) {
	s := settingsWith([]Agent{{Name: "a", Backend: BackendDevcontainer}}, nil)
	out, err := AddTrigger(s, Trigger{
		Name: "hook", Type: TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: WebhookFilterRegex, Regex: "push",
		Cron: "0 0 * * *", // should be cleared for a webhook trigger
	})
	if err != nil {
		t.Fatalf("AddTrigger webhook: %v", err)
	}
	if out.Triggers[0].Cron != "" {
		t.Errorf("schedule-only Cron should be cleared on a webhook trigger: %q", out.Triggers[0].Cron)
	}
}

func TestUpdateTriggerMissing(t *testing.T) {
	s := settingsWith([]Agent{{Name: "a", Backend: BackendDevcontainer}}, nil)
	if _, err := UpdateTrigger(s, "ghost", Trigger{Name: "ghost", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}); err == nil {
		t.Fatal("want not-found error")
	}
}

func TestUpdateTriggerRenameCollision(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "a", Backend: BackendDevcontainer}},
		[]Trigger{
			{Name: "t1", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"},
			{Name: "t2", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"},
		},
	)
	if _, err := UpdateTrigger(s, "t1", Trigger{Name: "t2", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}); err == nil {
		t.Fatal("want collision error")
	}
}

func TestDeleteTrigger(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "a", Backend: BackendDevcontainer}},
		[]Trigger{{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	)
	out, err := DeleteTrigger(s, "t")
	if err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}
	if len(out.Triggers) != 0 {
		t.Errorf("trigger not deleted: %+v", out.Triggers)
	}
	// Deleting a trigger frees its agent for deletion.
	if _, err := DeleteAgent(out, "a"); err != nil {
		t.Errorf("agent should be deletable once unreferenced: %v", err)
	}
}

func TestDeleteTriggerMissing(t *testing.T) {
	if _, err := DeleteTrigger(FleetSettings{}, "ghost"); err == nil {
		t.Fatal("want not-found error")
	}
}

func TestFindAgentAndTrigger(t *testing.T) {
	s := settingsWith(
		[]Agent{{Name: "a", Backend: BackendDevcontainer, Command: "cmd"}},
		[]Trigger{{Name: "t", Type: TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
	)
	if a, ok := FindAgent(s.Agents, "a"); !ok || a.Command != "cmd" {
		t.Errorf("FindAgent: ok=%v a=%+v", ok, a)
	}
	if _, ok := FindAgent(s.Agents, "ghost"); ok {
		t.Error("FindAgent should miss an unknown name")
	}
	if tr, ok := FindTrigger(s.Triggers, "t"); !ok || !slices.Equal(tr.AgentNames, []string{"a"}) {
		t.Errorf("FindTrigger: ok=%v t=%+v", ok, tr)
	}
	if _, ok := FindTrigger(s.Triggers, "ghost"); ok {
		t.Error("FindTrigger should miss an unknown name")
	}
}
