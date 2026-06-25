package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestCreateAutomationInstanceMarksAutomated(t *testing.T) {
	// The real scheduler create path must persist Automated=true so the TUI can
	// badge the instance. Stub the async provisioning job to a no-op so only the
	// synchronous record creation is under test.
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	orig := jobRunCreate
	jobRunCreate = func(string, string, string, string, bool, fleet.BackendType) error { return nil }
	defer func() { jobRunCreate = orig }()

	s, _, cleanup := newTestServer(t)
	defer cleanup()

	name, err := createAutomationInstance(s, "alpha", fleet.Agent{Name: "builder", Backend: fleet.BackendDevcontainer}, time.Now())
	if err != nil {
		t.Fatalf("createAutomationInstance: %v", err)
	}

	st, _ := state.Load()
	inst, err := st.Fleets["alpha"].GetInstance(name)
	if err != nil {
		t.Fatalf("instance %q not found: %v", name, err)
	}
	if !inst.Automated {
		t.Fatalf("scheduler-spawned instance should be marked Automated: %+v", inst)
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
	due := dueCronTriggers(fleets, now, lastFired)
	if len(due) != 1 || due[0].trigger.Name != "match" {
		t.Fatalf("want only 'match' to fire, got %+v", due)
	}

	// Same minute again: no re-fire.
	if again := dueCronTriggers(fleets, now.Add(20*time.Second), lastFired); len(again) != 0 {
		t.Fatalf("trigger re-fired within the same minute: %+v", again)
	}

	// Next matching week: fires again.
	next := now.AddDate(0, 0, 7)
	if again := dueCronTriggers(fleets, next, lastFired); len(again) != 1 {
		t.Fatalf("trigger should fire on the next matching minute: %+v", again)
	}
}

func TestDueSchedulesSkipsDisabled(t *testing.T) {
	// 2026-06-22 09:00 is a Monday; both triggers match the minute, but the
	// disabled one must never fire.
	now := time.Date(2026, 6, 22, 9, 0, 30, 0, time.UTC)
	fleets := scheduleFleet([]fleet.Trigger{
		{Name: "on", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 9 * * 1"},
		{Name: "off", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 9 * * 1", Disabled: true},
	}, []fleet.Agent{{Name: "a"}})

	lastFired := map[string]time.Time{}
	due := dueCronTriggers(fleets, now, lastFired)
	if len(due) != 1 || due[0].trigger.Name != "on" {
		t.Fatalf("want only the enabled trigger to fire, got %+v", due)
	}
	// A disabled trigger should not even be recorded as fired, so re-enabling it
	// later fires on the next matching minute rather than being suppressed.
	if _, ok := lastFired["alpha\x00off"]; ok {
		t.Fatal("disabled trigger should not record a lastFired entry")
	}
}

func TestDueSchedulesPrunesStaleLastFired(t *testing.T) {
	now := time.Date(2026, 6, 22, 9, 0, 30, 0, time.UTC) // Monday 09:00
	fleets := scheduleFleet([]fleet.Trigger{
		{Name: "match", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 9 * * 1"},
	}, []fleet.Agent{{Name: "a"}})

	lastFired := map[string]time.Time{}
	dueCronTriggers(fleets, now, lastFired)
	if _, ok := lastFired["alpha\x00match"]; !ok {
		t.Fatal("expected lastFired entry for the fired trigger")
	}
	// A stale entry (trigger that no longer exists) plus the live one.
	lastFired["alpha\x00deleted"] = now
	dueCronTriggers(fleets, now.Add(time.Minute), lastFired)
	if _, ok := lastFired["alpha\x00deleted"]; ok {
		t.Fatal("stale lastFired entry should have been pruned")
	}
	if _, ok := lastFired["alpha\x00match"]; !ok {
		t.Fatal("live trigger's lastFired entry must be kept")
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

func newWatchedScheduler(now time.Time) *scheduler {
	sched := newScheduler()
	sched.watched["alpha/agent-1"] = &watchedAgent{
		fleet: "alpha", instance: "agent-1",
		spawnedAt: now, lastActive: now,
	}
	return sched
}

func TestServiceWatchedLaunchesWhenRunning(t *testing.T) {
	rec := stubAutomationSeams(t)
	s := &service{}
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	sched := newWatchedScheduler(now)

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
	sched := newWatchedScheduler(t0)
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

func TestSchedulerTickKeepsWatchAfterFiring(t *testing.T) {
	// Regression: schedulerTick used the pre-fire state snapshot for
	// serviceWatched, so the just-created instance wasn't visible and the watch
	// entry was deleted the same tick — the agent never launched.
	rec := stubAutomationSeams(t)

	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "file:///x", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "echoer", Backend: fleet.BackendDevcontainer}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"echoer"}, Cron: "* * * * *", Prompt: "hi"}},
		}},
	}}

	origLoad := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	origCreate := createAutomationInstance
	// Mimic startCreateInstanceJob: synchronously add a StatusCreating record.
	createAutomationInstance = func(_ *service, fleetName string, _ fleet.Agent, _ time.Time) (string, error) {
		st.Fleets[fleetName].Instances = append(st.Fleets[fleetName].Instances,
			&fleet.Instance{Name: "echoer-inst", Status: fleet.StatusCreating, ContainerID: "c1"})
		return "echoer-inst", nil
	}
	t.Cleanup(func() { scheduleLoadState = origLoad; createAutomationInstance = origCreate })

	s := &service{}
	sched := newScheduler()
	now := time.Date(2026, 6, 22, 9, 0, 30, 0, time.UTC)

	// Tick 1: fires + creates the (Creating) instance. The watch entry must
	// survive — not be deleted by serviceWatched running on stale state.
	s.schedulerTick(context.Background(), sched, now)
	w, ok := sched.watched["alpha/echoer-inst"]
	if !ok {
		t.Fatal("watch entry must survive the firing tick")
	}
	if w.launched || len(rec.launched) != 0 {
		t.Fatal("agent must not launch while the instance is still Creating")
	}

	// Tick 2 (same minute → no re-fire): instance now Running → agent launches.
	st.Fleets["alpha"].Instances[0].Status = fleet.StatusRunning
	s.schedulerTick(context.Background(), sched, now.Add(10*time.Second))
	if len(rec.launched) != 1 || rec.launched[0] != "echoer-inst" {
		t.Fatalf("agent should launch once the instance is running, got %v", rec.launched)
	}
}

func TestBuildAgentLaunchScript(t *testing.T) {
	// The substituted command (prompt already inside it) must run via an
	// interactive bash so ~/.bashrc is sourced and the agent (e.g. ~/.local/bin/
	// claude) is found — a bare tmux `sh -c` does not source it and the session
	// dies instantly.
	cmd := fleet.SubstituteAgentCommand(fleet.DefaultAgentCommand, "do it", "be terse")
	script := buildAgentLaunchScript("inst~agent", cmd)
	for _, want := range []string{
		"tmux new-session -d -s 'inst~agent'",
		"bash -ic ",
		// Both prompts are carried by the command (single-quote-escaped); no
		// send-keys.
		"be terse",
		"do it",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("launch script missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "send-keys") {
		t.Fatalf("launch should not use send-keys anymore:\n%s", script)
	}
}

func TestServiceWatchedConcurrentProbesReapAll(t *testing.T) {
	rec := stubAutomationSeams(t)
	rec.activity = func(time.Time) (agentdetect.State, bool) { return agentdetect.StateWaiting, true }
	s := &service{}
	t0 := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)

	// Several idle agents probed concurrently must all reap, with the watch set
	// fully drained (no double-count / drop under the fan-out).
	const n = 5
	sched := newScheduler()
	insts := make([]*fleet.Instance, 0, n)
	for i := 0; i < n; i++ {
		name := automationInstanceName("agent", t0.Add(time.Duration(i)*time.Second))
		insts = append(insts, &fleet.Instance{Name: name, Status: fleet.StatusRunning, ContainerID: "c"})
		sched.watched["alpha/"+name] = &watchedAgent{
			fleet: "alpha", instance: name, launched: true,
			spawnedAt: t0, lastActive: t0,
		}
	}
	st := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: insts}}}

	s.serviceWatched(context.Background(), sched, st, t0.Add(automationIdleTimeout))
	if len(rec.reaped) != n {
		t.Fatalf("expected %d reaps, got %d (%v)", n, len(rec.reaped), rec.reaped)
	}
	if len(sched.watched) != 0 {
		t.Fatalf("watch set should be drained, still has %d", len(sched.watched))
	}
}

// TestFireWebhookBatchSpawns exercises the webhook critical path on the scheduler
// goroutine: a delivered fire batch resolves the trigger's agents and registers
// each as a watched instance (the same lifecycle scheduled triggers use).
func TestFireWebhookBatchSpawns(t *testing.T) {
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents: []fleet.Agent{
				{Name: "a", Command: "cmdA", SystemPrompt: "sysA", Backend: fleet.BackendDevcontainer},
				{Name: "b", Command: "cmdB", Backend: fleet.BackendDevcontainer},
			},
		}},
	}}
	origLoad := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	origCreate := createAutomationInstance
	var created []string
	createAutomationInstance = func(_ *service, _ string, ag fleet.Agent, _ time.Time) (string, error) {
		name := "inst-" + ag.Name
		created = append(created, name)
		return name, nil
	}
	t.Cleanup(func() { scheduleLoadState = origLoad; createAutomationInstance = origCreate })

	s := &service{}
	sched := newScheduler()
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)

	// One trigger activating both agents, with a prompt.
	trig := fleet.Trigger{Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a", "b"}, WebhookName: "ci", Prompt: "go"}
	s.fireTriggerBatch(sched, []triggerFire{{fleet: "alpha", trigger: trig}}, now)

	if len(created) != 2 {
		t.Fatalf("expected 2 instances created, got %v", created)
	}
	wa := sched.watched["alpha/inst-a"]
	if wa == nil {
		t.Fatal("agent a was not registered in the watch set")
	}
	if wa.command != "cmdA" || wa.systemPrompt != "sysA" || wa.prompt != "go" {
		t.Fatalf("watched agent a carries the wrong fields: %+v", wa)
	}
	if sched.watched["alpha/inst-b"] == nil {
		t.Fatal("agent b was not registered in the watch set")
	}
}

// TestFireWebhookBatchSkipsMissingAgents confirms a fire whose trigger references
// an agent that no longer exists is skipped (no crash, no watch entry), per fleet.
func TestFireWebhookBatchSkipsMissingAgents(t *testing.T) {
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{Agents: nil}}, // no agents
	}}
	origLoad := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	origCreate := createAutomationInstance
	createAutomationInstance = func(*service, string, fleet.Agent, time.Time) (string, error) {
		t.Fatal("createAutomationInstance must not run when the trigger has no live agents")
		return "", nil
	}
	t.Cleanup(func() { scheduleLoadState = origLoad; createAutomationInstance = origCreate })

	s := &service{}
	sched := newScheduler()
	trig := fleet.Trigger{Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"gone"}, WebhookName: "ci"}
	s.fireTriggerBatch(sched, []triggerFire{{fleet: "alpha", trigger: trig}}, time.Now())
	if len(sched.watched) != 0 {
		t.Fatalf("no watch entries expected, got %d", len(sched.watched))
	}
}

// TestDueCronTriggersIncludesBash confirms the shared cron sampler returns BOTH
// schedule and bash triggers (they are the cron-driven types) and skips webhooks.
func TestDueCronTriggersIncludesBash(t *testing.T) {
	now := time.Date(2026, 6, 22, 9, 0, 30, 0, time.UTC) // Monday 09:00
	fleets := scheduleFleet([]fleet.Trigger{
		{Name: "sched", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 9 * * 1"},
		{Name: "bash", Type: fleet.TriggerBash, AgentNames: []string{"a"}, Cron: "0 9 * * 1", Script: "true"},
		{Name: "wh", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "x"},
	}, []fleet.Agent{{Name: "a"}})

	due := dueCronTriggers(fleets, now, map[string]time.Time{})
	names := map[string]bool{}
	for _, d := range due {
		names[d.trigger.Name] = true
	}
	if len(due) != 2 || !names["sched"] || !names["bash"] {
		t.Fatalf("expected schedule + bash to be due (webhook excluded), got %+v", due)
	}
}

// TestSchedulerTickBashProbe confirms a due bash trigger is probed (off the tick)
// rather than spawning an instance directly the way a schedule trigger does.
func TestSchedulerTickBashProbe(t *testing.T) {
	stubAutomationSeams(t) // serviceWatched runs against an empty watch set

	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents: []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}},
			Triggers: []fleet.Trigger{
				{Name: "poll", Type: fleet.TriggerBash, AgentNames: []string{"a"}, Cron: "* * * * *", Script: "true"},
			},
		}},
	}}
	origLoad := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	origCreate := createAutomationInstance
	createAutomationInstance = func(*service, string, fleet.Agent, time.Time) (string, error) {
		t.Fatal("a bash trigger must not create an instance from the tick (only on a passing probe)")
		return "", nil
	}
	origProbe := startBashProbe
	var probed []string
	startBashProbe = func(_ *service, fleetName string, trigger fleet.Trigger, _ time.Time) {
		probed = append(probed, fleetName+"/"+trigger.Name)
	}
	t.Cleanup(func() {
		scheduleLoadState = origLoad
		createAutomationInstance = origCreate
		startBashProbe = origProbe
	})

	s := &service{}
	sched := newScheduler()
	s.schedulerTick(context.Background(), sched, time.Date(2026, 6, 22, 9, 0, 30, 0, time.UTC))

	if len(probed) != 1 || probed[0] != "alpha/poll" {
		t.Fatalf("expected the bash trigger to be probed once, got %v", probed)
	}
	if len(sched.watched) != 0 {
		t.Fatalf("no watch entry should exist until the probe fires, got %d", len(sched.watched))
	}
}

// TestStartBashProbe drives the real probe: a zero exit hands a fire (carrying the
// command's stdout as the body) to the scheduler via triggerFires; a non-zero exit
// fires nothing.
func TestStartBashProbe(t *testing.T) {
	orig := runBashScript
	t.Cleanup(func() { runBashScript = orig })

	s := &service{bgCtx: context.Background(), triggerFires: make(chan []triggerFire, 1)}
	trig := fleet.Trigger{Name: "poll", Type: fleet.TriggerBash, AgentNames: []string{"a"}, Cron: "* * * * *", Script: "echo hi"}

	// Zero exit → fires, carrying stdout as the payload body.
	runBashScript = func(_ context.Context, script string) ([]byte, bool, error) {
		if script != "echo hi" {
			t.Errorf("probe ran the wrong script: %q", script)
		}
		return []byte("payload-out\n"), true, nil
	}
	startBashProbe(s, "alpha", trig, time.Now())
	select {
	case batch := <-s.triggerFires:
		if len(batch) != 1 || batch[0].fleet != "alpha" || batch[0].trigger.Name != "poll" {
			t.Fatalf("unexpected fire batch: %+v", batch)
		}
		if string(batch[0].body) != "payload-out\n" {
			t.Fatalf("body = %q, want the command's stdout", batch[0].body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a fire on a zero exit")
	}

	// Non-zero exit → nothing fires. A non-firing probe sends nothing on
	// triggerFires, so signal completion via `ran` to synchronize with the probe
	// goroutine (otherwise the deferred runBashScript restore races its read).
	ran := make(chan struct{}, 1)
	runBashScript = func(context.Context, string) ([]byte, bool, error) {
		defer func() { ran <- struct{}{} }()
		return []byte("ignored"), false, errors.New("exit status 1")
	}
	startBashProbe(s, "alpha", trig, time.Now())
	<-ran // the probe ran its (non-zero) command; it will not fire
	select {
	case batch := <-s.triggerFires:
		t.Fatalf("a non-zero exit must not fire, got %+v", batch)
	default:
		// good: nothing queued
	}
}

// TestRunBashScript exercises the real exec seam: stdout is captured as the
// payload IN ISOLATION from stderr, and a non-zero exit reports failure with the
// stderr tail folded into the error.
func TestRunBashScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ctx := context.Background()

	out, ok, err := runBashScript(ctx, "printf payload; printf oops >&2; exit 0")
	if !ok || err != nil {
		t.Fatalf("zero exit: ok=%v err=%v", ok, err)
	}
	if string(out) != "payload" {
		t.Fatalf("stdout = %q, want %q (stderr must be excluded from the payload)", out, "payload")
	}

	out, ok, err = runBashScript(ctx, "echo nope >&2; exit 3")
	if ok || err == nil {
		t.Fatalf("non-zero exit: want failure, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should carry the stderr tail, got: %v", err)
	}
}

// TestRunBashScriptBackgroundedGrandchildTimesOut guards the WaitDelay fix: a
// command that backgrounds a long-lived process which inherits the stdout pipe
// must NOT block Run() for that process's whole lifetime, and must be reported as
// not-fired (CommandContext kills only the direct shell). Without WaitDelay this
// blocks for the full `sleep` and falsely reports a zero exit.
func TestRunBashScriptBackgroundedGrandchildTimesOut(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, ok, err := runBashScript(ctx, "sleep 30 & echo hi")
	elapsed := time.Since(start)

	// WaitDelay (3s) bounds how long Run lingers after the deadline; 30s would mean
	// the deadline was ignored entirely.
	if elapsed > 10*time.Second {
		t.Fatalf("probe blocked %v on a backgrounded grandchild — WaitDelay not bounding it", elapsed)
	}
	if ok || err == nil {
		t.Fatalf("a timed-out probe must be reported as not-fired, got ok=%v err=%v", ok, err)
	}
}

func TestEnvDurationDefault(t *testing.T) {
	const name = "FLEET_TEST_DURATION_KNOB"
	def := 2 * time.Minute
	cases := []struct {
		val  string
		set  bool
		want time.Duration
	}{
		{set: false, want: def},             // unset → default
		{val: "", set: true, want: def},     // blank → default
		{val: "  ", set: true, want: def},   // whitespace → default
		{val: "nope", set: true, want: def}, // unparseable → default
		{val: "0s", set: true, want: def},   // non-positive → default
		{val: "-5s", set: true, want: def},  // negative → default
		{val: "20s", set: true, want: 20 * time.Second},
		{val: "1m30s", set: true, want: 90 * time.Second},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv(name, c.val)
		} else {
			os.Unsetenv(name)
		}
		if got := envDurationDefault(name, def); got != c.want {
			t.Fatalf("envDurationDefault(%q set=%v) = %v, want %v", c.val, c.set, got, c.want)
		}
	}
}

func TestServiceWatchedDropsGoneInstance(t *testing.T) {
	stubAutomationSeams(t)
	s := &service{}
	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	sched := newWatchedScheduler(now)

	// Instance not present in state (destroyed) → dropped from the watch set.
	s.serviceWatched(context.Background(), sched, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}, now)
	if _, still := sched.watched["alpha/agent-1"]; still {
		t.Fatal("vanished instance should be dropped from the watch set")
	}
}
