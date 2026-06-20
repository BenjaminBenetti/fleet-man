package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func scheduleFleet(triggers []fleet.Trigger, agents []fleet.Agent) map[string]*fleet.Fleet {
	return map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{Triggers: triggers, Agents: agents}},
	}
}

func TestDueSchedulesFiresOncePerMinute(t *testing.T) {
	// 2026-06-22 is a Monday at 09:00.
	now := time.Date(2026, 6, 22, 9, 0, 30, 0, time.UTC)
	fleets := scheduleFleet([]fleet.Trigger{
		{Name: "match", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 9 * * 1"},
		{Name: "nomatch", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 10 * * 1"},
		{Name: "webhook", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "x"},
		{Name: "badcron", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "not cron"},
	}, []fleet.Agent{{Name: "a"}})

	lastFired := map[string]time.Time{}
	due := dueSchedules(fleets, now, lastFired)
	if len(due) != 1 || due[0].trigger.Name != "match" {
		t.Fatalf("want only 'match' to fire, got %+v", due)
	}

	// Same minute again: no re-fire.
	if again := dueSchedules(fleets, now.Add(20*time.Second), lastFired); len(again) != 0 {
		t.Fatalf("trigger re-fired within the same minute: %+v", again)
	}

	// Next matching week: fires again.
	next := now.AddDate(0, 0, 7)
	if again := dueSchedules(fleets, next, lastFired); len(again) != 1 {
		t.Fatalf("trigger should fire on the next matching minute: %+v", again)
	}
}

func TestAgentsForTrigger(t *testing.T) {
	f := &fleet.Fleet{Settings: fleet.FleetSettings{Agents: []fleet.Agent{{Name: "a"}, {Name: "b"}}}}
	got := agentsForTrigger(f, fleet.Trigger{AgentNames: []string{"b", "ghost", "a"}})
	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("agentsForTrigger = %+v", got)
	}
}

func TestAutomationInstanceName(t *testing.T) {
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	n1 := automationInstanceName("My Builder!", now)
	n2 := automationInstanceName("My Builder!", now)
	if n1 == n2 {
		t.Fatalf("names should be unique: %q == %q", n1, n2)
	}
	for _, n := range []string{n1, n2} {
		if err := fleet.ValidateInstanceName(n); err != nil {
			t.Fatalf("invalid instance name %q: %v", n, err)
		}
		if !strings.HasPrefix(n, "my-builder-") {
			t.Fatalf("sanitized name unexpected: %q", n)
		}
	}
}

// stubAutomationSeams overrides the live operation seams for the duration of a
// test, returning a restore function and recorders.
type seamRecorder struct {
	launched []string
	reaped   []string
	activity func(now time.Time) (agentdetect.State, bool)
}

func stubAutomationSeams(t *testing.T) *seamRecorder {
	t.Helper()
	rec := &seamRecorder{activity: func(time.Time) (agentdetect.State, bool) { return agentdetect.StateWaiting, true }}

	origLaunch := launchAutomationCommand
	origActivity := automationActivity
	origReap := reapAutomationInstance
	launchAutomationCommand = func(_ context.Context, _ *service, w *watchedAgent, _ *fleet.Instance) {
		rec.launched = append(rec.launched, w.instance)
	}
	automationActivity = func(_ *service, _ *watchedAgent, _ *fleet.Instance, now time.Time) (agentdetect.State, bool) {
		return rec.activity(now)
	}
	reapAutomationInstance = func(_ *service, _, instanceName string) {
		rec.reaped = append(rec.reaped, instanceName)
	}
	t.Cleanup(func() {
		launchAutomationCommand = origLaunch
		automationActivity = origActivity
		reapAutomationInstance = origReap
	})
	return rec
}

func watchedState(status fleet.InstanceStatus) *state.State {
	return &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "agent-1", Status: status, ContainerID: "c1"},
		}},
	}}
}

func newWatchedScheduler(now time.Time, tmux bool) *scheduler {
	sched := newScheduler()
	sched.watched["alpha/agent-1"] = &watchedAgent{
		fleet: "alpha", instance: "agent-1", tmux: tmux,
		spawnedAt: now, lastActive: now,
		detector: agentdetect.NewTmuxPaneChangeDetector(),
	}
	return sched
}

func TestServiceWatchedLaunchesWhenRunning(t *testing.T) {
	rec := stubAutomationSeams(t)
	s := &service{}
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	sched := newWatchedScheduler(now, true)

	// Still creating → no launch.
	s.serviceWatched(context.Background(), sched, watchedState(fleet.StatusCreating), now)
	if len(rec.launched) != 0 {
		t.Fatalf("should not launch before running: %v", rec.launched)
	}

	// Running → launch once, mark launched.
	s.serviceWatched(context.Background(), sched, watchedState(fleet.StatusRunning), now)
	if len(rec.launched) != 1 || rec.launched[0] != "agent-1" {
		t.Fatalf("expected one launch, got %v", rec.launched)
	}
	if !sched.watched["alpha/agent-1"].launched {
		t.Fatal("watched agent should be marked launched")
	}
}

func TestServiceWatchedReapsIdle(t *testing.T) {
	rec := stubAutomationSeams(t)
	s := &service{}
	t0 := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	sched := newWatchedScheduler(t0, true)
	sched.watched["alpha/agent-1"].launched = true
	st := watchedState(fleet.StatusRunning)

	// Working keeps it alive and resets the idle clock.
	rec.activity = func(time.Time) (agentdetect.State, bool) { return agentdetect.StateWorking, true }
	s.serviceWatched(context.Background(), sched, st, t0.Add(time.Minute))
	if len(rec.reaped) != 0 {
		t.Fatalf("working agent must not be reaped: %v", rec.reaped)
	}

	// Idle but within the timeout → not reaped.
	rec.activity = func(time.Time) (agentdetect.State, bool) { return agentdetect.StateWaiting, true }
	s.serviceWatched(context.Background(), sched, st, t0.Add(2*time.Minute))
	if len(rec.reaped) != 0 {
		t.Fatalf("agent reaped too early: %v", rec.reaped)
	}

	// Idle past the timeout (measured from the last Working at t0+1m) → reaped.
	s.serviceWatched(context.Background(), sched, st, t0.Add(time.Minute+automationIdleTimeout))
	if len(rec.reaped) != 1 || rec.reaped[0] != "agent-1" {
		t.Fatalf("expected reap, got %v", rec.reaped)
	}
	if _, still := sched.watched["alpha/agent-1"]; still {
		t.Fatal("reaped agent should be dropped from the watch set")
	}
}

func TestServiceWatchedNonTmuxNotReaped(t *testing.T) {
	rec := stubAutomationSeams(t)
	rec.activity = func(time.Time) (agentdetect.State, bool) { return agentdetect.StateWaiting, true }
	s := &service{}
	t0 := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	sched := newWatchedScheduler(t0, false) // non-tmux
	sched.watched["alpha/agent-1"].launched = true

	s.serviceWatched(context.Background(), sched, watchedState(fleet.StatusRunning), t0.Add(time.Hour))
	if len(rec.reaped) != 0 {
		t.Fatalf("non-tmux agents must never be idle-reaped: %v", rec.reaped)
	}
}

func TestServiceWatchedDropsGoneInstance(t *testing.T) {
	stubAutomationSeams(t)
	s := &service{}
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	sched := newWatchedScheduler(now, true)

	// Instance not present in state (destroyed) → dropped from the watch set.
	s.serviceWatched(context.Background(), sched, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}, now)
	if _, still := sched.watched["alpha/agent-1"]; still {
		t.Fatal("vanished instance should be dropped from the watch set")
	}
}
