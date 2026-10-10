package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// persistedScheduler is a scheduler that persists to the isolated HOME's
// watch.json, as the serve loop's does.
func persistedScheduler() *scheduler {
	sched := newScheduler()
	sched.watchPath = state.AutomationWatchPath()
	return sched
}

// stubScheduleState points the scheduler's state reads at st.
func stubScheduleState(t *testing.T, st *state.State) {
	t.Helper()
	orig := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	t.Cleanup(func() { scheduleLoadState = orig })
}

func readPersistedWatch(t *testing.T) *watchFile {
	t.Helper()
	wf, existed, err := readWatchFile(state.AutomationWatchPath())
	if err != nil || !existed {
		t.Fatalf("watch.json: existed=%v err=%v", existed, err)
	}
	return wf
}

// TestWatchListRoundTrip: what one daemon persists, the next restores — every
// field a run needs to launch or be reaped, the payload of a run still to
// launch (read back exactly from its trigger event log, trailing newline or
// not), and the minutes cron triggers last fired. The idle clock restarts.
func TestWatchListRoundTrip(t *testing.T) {
	isolateFleetDir(t)
	fired := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	ev := &triggerEvent{kind: fleet.TriggerWebhook, triggerName: "ci", firedAt: fired, webhookName: "gh", body: []byte(`{"no":"newline"}`)}

	sched := persistedScheduler()
	sched.watched["alpha/pending"] = &watchedAgent{
		fleet: "alpha", instance: "pending", agent: "writer",
		command: "claude '${PROMPT}'", prompt: "go", systemPrompt: "terse",
		spawnedAt: fired, lastActive: fired, event: ev, eventLog: logTriggerEvent("alpha", ev),
	}
	sched.watched["alpha/running"] = &watchedAgent{
		fleet: "alpha", instance: "running", agent: "writer", command: "c",
		spawnedAt: fired, lastActive: fired, launched: true,
		event: &triggerEvent{kind: fleet.TriggerSchedule, triggerName: "nightly", firedAt: fired},
	}
	sched.lastFired[firedKey("alpha", "nightly")] = fired
	sched.persist()

	stubScheduleState(t, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
		{Name: "pending", Status: fleet.StatusCreating}, {Name: "running", Status: fleet.StatusRunning},
	}}}})
	now := fired.Add(time.Hour)
	restored := persistedScheduler()
	restored.restore(now)

	p := restored.watched["alpha/pending"]
	if p == nil {
		t.Fatal("unlaunched run not restored")
	}
	if p.agent != "writer" || p.command != "claude '${PROMPT}'" || p.prompt != "go" || p.systemPrompt != "terse" || p.launched || p.adopted {
		t.Fatalf("restored run = %+v", p)
	}
	if p.event == nil || p.event.kind != fleet.TriggerWebhook || p.event.triggerName != "ci" || p.event.webhookName != "gh" || !p.event.firedAt.Equal(fired) {
		t.Fatalf("restored event = %+v", p.event)
	}
	if string(p.event.body) != `{"no":"newline"}` {
		t.Fatalf("restored body = %q, want the exact payload", p.event.body)
	}
	if !p.lastActive.Equal(now) || !p.spawnedAt.Equal(fired) {
		t.Fatalf("clocks: lastActive=%v spawnedAt=%v, want the idle clock restarted at %v", p.lastActive, p.spawnedAt, now)
	}
	if r := restored.watched["alpha/running"]; r == nil || !r.launched || r.event.triggerName != "nightly" {
		t.Fatalf("launched run restored as %+v", r)
	}
	if m := restored.lastFired[firedKey("alpha", "nightly")]; !m.Equal(fired) {
		t.Fatalf("lastFired = %v, want %v", m, fired)
	}
}

// TestRestoreWatchedDropsGoneAndAdoptsUnwatched: a run whose instance is gone
// is dropped; an Automated instance the file does not know is adopted (as
// launched, so its agent is never started twice); failed and user instances
// are left alone. The reconciled list is written back.
func TestRestoreWatchedDropsGoneAndAdoptsUnwatched(t *testing.T) {
	isolateFleetDir(t)
	t0 := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	seed := persistedScheduler()
	seed.watched["alpha/known"] = &watchedAgent{fleet: "alpha", instance: "known", command: "c", spawnedAt: t0, launched: true}
	seed.watched["alpha/gone"] = &watchedAgent{fleet: "alpha", instance: "gone", command: "c", spawnedAt: t0}
	seed.persist()

	stubScheduleState(t, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
		{Name: "known", Status: fleet.StatusRunning, Automated: true},
		{Name: "orphan", Status: fleet.StatusRunning, Automated: true, CreatedAt: t0},
		{Name: "dead", Status: fleet.StatusFailed, Automated: true},
		{Name: "mine", Status: fleet.StatusRunning},
	}}}})
	sched := persistedScheduler()
	sched.restore(t0.Add(time.Hour))

	if len(sched.watched) != 2 {
		t.Fatalf("watched = %v, want known + orphan", keysOf(sched.watched))
	}
	if k := sched.watched["alpha/known"]; k == nil || k.adopted {
		t.Fatalf("known run = %+v, want restored, not adopted", k)
	}
	o := sched.watched["alpha/orphan"]
	if o == nil || !o.adopted || !o.launched || !o.spawnedAt.Equal(t0) {
		t.Fatalf("orphan = %+v, want adopted as launched", o)
	}

	wf := readPersistedWatch(t)
	got := map[string]watchRun{}
	for _, r := range wf.Runs {
		got[r.Instance] = r
	}
	if len(got) != 2 || !got["orphan"].Adopted || got["known"].Adopted {
		t.Fatalf("written back = %+v", wf.Runs)
	}
}

func keysOf(m map[string]*watchedAgent) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAdoptedRunIdleLimit: an adopted run is reaped on the idle rule while its
// agent session exists, and only after automationAdoptGrace once it has none.
func TestAdoptedRunIdleLimit(t *testing.T) {
	rec := stubAutomationSeams(t)
	t0 := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		hasSession bool
		wantLimit  time.Duration
	}{
		{"with agent session", true, automationIdleTimeout},
		{"without agent session", false, automationAdoptGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec.reaped = nil
			automationActivity = func(_ *service, w *watchedAgent, _ *fleet.Instance, _ time.Time) (agentdetect.State, bool) {
				w.hasAgentSession = tc.hasSession
				return agentdetect.StateWaiting, true
			}
			sched := newScheduler()
			sched.watched["alpha/agent-1"] = &watchedAgent{fleet: "alpha", instance: "agent-1", launched: true, adopted: true, lastActive: t0}
			st := watchedState(fleet.StatusRunning)

			(&service{}).serviceWatched(context.Background(), sched, st, t0.Add(tc.wantLimit-time.Second))
			if len(rec.reaped) != 0 {
				t.Fatalf("reaped before %v", tc.wantLimit)
			}
			(&service{}).serviceWatched(context.Background(), sched, st, t0.Add(tc.wantLimit))
			if len(rec.reaped) != 1 {
				t.Fatalf("not reaped after %v idle", tc.wantLimit)
			}
		})
	}
}

// TestRestoredFireMinuteIsNotFiredAgain: a daemon restarted within the minute
// a cron trigger fired does not fire it (or poll its bash command) again.
func TestRestoredFireMinuteIsNotFiredAgain(t *testing.T) {
	isolateFleetDir(t)
	stubAutomationSeams(t)
	now := time.Date(2026, 6, 22, 9, 0, 10, 0, time.UTC)
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a", Command: "c", Backend: fleet.BackendDevcontainer}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}},
		}},
	}}
	stubScheduleState(t, st)
	created := 0
	origCreate := createAutomationInstance
	createAutomationInstance = func(_ *service, fleetName string, _ fleet.Agent, _ time.Time) (string, error) {
		created++
		st.Fleets[fleetName].Instances = append(st.Fleets[fleetName].Instances, &fleet.Instance{Name: "run-1", Status: fleet.StatusCreating, Automated: true})
		return "run-1", nil
	}
	t.Cleanup(func() { createAutomationInstance = origCreate })

	first := persistedScheduler()
	(&service{}).schedulerTick(context.Background(), first, now)
	if created != 1 {
		t.Fatalf("created %d instances, want 1", created)
	}
	wf := readPersistedWatch(t)
	if len(wf.Runs) != 1 || wf.Runs[0].Instance != "run-1" || wf.Runs[0].Agent != "a" || wf.Runs[0].Trigger != "t" {
		t.Fatalf("persisted runs = %+v, want the new run", wf.Runs)
	}

	// The daemon restarts 20s later, within the same minute.
	second := persistedScheduler()
	second.restore(now.Add(20 * time.Second))
	(&service{}).schedulerTick(context.Background(), second, now.Add(20*time.Second))
	if created != 1 {
		t.Fatalf("the restarted daemon fired the trigger again (%d creates)", created)
	}
	if second.watched["alpha/run-1"] == nil {
		t.Fatal("the restarted daemon lost the run")
	}
}

// TestPersistWritesAPrivateFileOnlyOnChange: watch.json is 0600 in a 0700
// directory, and an unchanged list is not rewritten.
func TestPersistWritesAPrivateFileOnlyOnChange(t *testing.T) {
	isolateFleetDir(t)
	sched := persistedScheduler()
	sched.watched["alpha/a"] = &watchedAgent{fleet: "alpha", instance: "a", command: "c"}
	sched.persist()

	path := state.AutomationWatchPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("watch.json mode = %v, want 0600", info.Mode().Perm())
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Fatalf("automation dir mode = %v, want 0700", dir.Mode().Perm())
	}

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	sched.persist()
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatal("an unchanged watch list was rewritten")
	}
	sched.watched["alpha/a"].launched = true
	sched.persist()
	if wf := readPersistedWatch(t); len(wf.Runs) != 1 || !wf.Runs[0].Launched {
		t.Fatalf("launch not persisted: %+v", wf.Runs)
	}
}

// TestRestoreWatchedStartsEmptyOnABadFile: an unreadable or newer-versioned
// file is warned about and ignored, never misread.
func TestRestoreWatchedStartsEmptyOnABadFile(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt": "{not json",
		"newer":   `{"v": 99, "runs": [{"fleet": "alpha", "instance": "x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolateFleetDir(t)
			stubScheduleState(t, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "x", Status: fleet.StatusRunning}}}}})
			if err := writeWatchFile(state.AutomationWatchPath(), []byte(content)); err != nil {
				t.Fatal(err)
			}
			sched := persistedScheduler()
			sched.restore(time.Now())
			if len(sched.watched) != 0 {
				t.Fatalf("restored %v from a bad file", keysOf(sched.watched))
			}
		})
	}
}

// TestRestoreWithoutAFileWritesNothing: a daemon that never ran automation
// does not grow a watch.json.
func TestRestoreWithoutAFileWritesNothing(t *testing.T) {
	isolateFleetDir(t)
	stubScheduleState(t, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}})
	persistedScheduler().restore(time.Now())
	if _, err := os.Stat(state.AutomationWatchPath()); !os.IsNotExist(err) {
		t.Fatalf("watch.json created on an automation-free start: %v", err)
	}
}

// TestReadTriggerEventBody pins the payload read-back: exact bytes, and a
// refusal for a pruned log, a log of another firing, or a bad name.
func TestReadTriggerEventBody(t *testing.T) {
	isolateFleetDir(t)
	fired := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	for _, body := range []string{"no newline", "ends in one\n", "two\n\n", "# looks like a header\n\nx"} {
		ev := &triggerEvent{kind: fleet.TriggerBash, triggerName: "poll", firedAt: fired, body: []byte(body)}
		name := logTriggerEvent("alpha", ev)
		got, err := readTriggerEventBody("alpha", ev, name, len(body))
		if err != nil || string(got) != body {
			t.Fatalf("body %q read back as %q (err %v)", body, got, err)
		}
	}

	ev := &triggerEvent{kind: fleet.TriggerWebhook, triggerName: "ci", firedAt: fired, webhookName: "gh", body: []byte("x")}
	name := logTriggerEvent("alpha", ev)
	other := *ev
	other.firedAt = fired.Add(time.Minute)
	if _, err := readTriggerEventBody("alpha", &other, name, 1); err == nil {
		t.Fatal("a log of another firing was accepted")
	}
	if _, err := readTriggerEventBody("alpha", ev, name, 5); err == nil {
		t.Fatal("a payload longer than the log was accepted")
	}
	for _, bad := range []string{"", "../state.json", "x/event-1.log", "notes.txt"} {
		if _, err := readTriggerEventBody("alpha", ev, bad, 1); err == nil {
			t.Fatalf("bad log name %q accepted", bad)
		}
	}
	if err := os.Remove(filepath.Join(state.TriggerLogsDir("alpha", "ci"), name)); err != nil {
		t.Fatal(err)
	}
	if _, err := readTriggerEventBody("alpha", ev, name, 1); err == nil {
		t.Fatal("a pruned log was read")
	}
}
