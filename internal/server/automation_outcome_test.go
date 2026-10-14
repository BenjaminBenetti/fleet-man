package server

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// triggerLog is the trigger's recorded event logs, concatenated.
func triggerLog(t *testing.T, fleetName, trigger string) string {
	t.Helper()
	logs, _, err := readTriggerLogs(fleetName, trigger)
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func assertLogHas(t *testing.T, logs string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(logs, w) {
			t.Errorf("trigger log lacks %q:\n%s", w, logs)
		}
	}
}

// Every firing's outcome lands in its event log, on any backend: the
// instance each agent got or why it got none, a failed create with its
// reason, and the reap.
func TestFiringOutcomesAreLogged(t *testing.T) {
	isolateFleetDir(t)
	rec := stubAutomationSeams(t)
	st := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}
	stubScheduleState(t, st)
	origCreate := createAutomationInstance
	createAutomationInstance = func(_ *service, _ string, ag fleet.Agent, _ time.Time) (string, error) {
		if ag.Name == "broken" {
			return "", errors.New("devcontainer CLI not found")
		}
		name := "inst-" + ag.Name
		st.Fleets["alpha"].Instances = append(st.Fleets["alpha"].Instances, &fleet.Instance{Name: name, Status: fleet.StatusCreating})
		return name, nil
	}
	t.Cleanup(func() { createAutomationInstance = origCreate })

	s := &service{}
	sched := newScheduler()
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	trig := fleet.Trigger{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a", "b", "broken"}}
	agents := []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}, {Name: "b", Backend: fleet.BackendCoder}, {Name: "broken"}}
	if !s.fireTriggerAgents(sched, "alpha", trig, agents, t0, nil) {
		t.Fatal("nothing fired")
	}
	assertLogHas(t, triggerLog(t, "alpha", "nightly"),
		"# outcome: 2026-10-11T09:00:00Z a: instance inst-a (devcontainer)",
		"# outcome: 2026-10-11T09:00:00Z b: instance inst-b (coder)",
		"# outcome: 2026-10-11T09:00:00Z broken: not created: devcontainer CLI not found",
	)

	// inst-a's create fails; inst-b runs, goes idle and is reaped.
	st.Fleets["alpha"].Instances[0].Status, st.Fleets["alpha"].Instances[0].Error = fleet.StatusFailed, "devcontainer up: exit status 1"
	st.Fleets["alpha"].Instances[1].Status = fleet.StatusRunning
	s.serviceWatched(context.Background(), sched, st, t0.Add(time.Minute)) // inst-b launches
	s.serviceWatched(context.Background(), sched, st, t0.Add(time.Minute+automationIdleTimeout))
	if len(rec.reaped) != 1 || rec.reaped[0] != "inst-b" {
		t.Fatalf("reaped %v", rec.reaped)
	}
	assertLogHas(t, triggerLog(t, "alpha", "nightly"),
		"# outcome: 2026-10-11T09:01:00Z a: create of inst-a failed: devcontainer up: exit status 1",
		"b: instance inst-b reaped after 2m0s idle; it is being destroyed",
	)
	if _, running := sched.lim.counts(); len(running) != 0 {
		t.Fatalf("runs still hold slots: %v", running)
	}
}

// A failed cluster run gives its pod and disk back at once and keeps its
// record; one a daemon restart interrupted is destroyed. On another backend
// both keep their records, as before.
func TestFailedRunsOnAClusterLetGo(t *testing.T) {
	isolateFleetDir(t)
	rec := stubAutomationSeams(t)
	var released []string
	orig := releaseFailedRun
	releaseFailedRun = func(_ *service, _ string, inst *fleet.Instance) { released = append(released, inst.Name) }
	t.Cleanup(func() { releaseFailedRun = orig })

	s := &service{}
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	ev := &triggerEvent{kind: fleet.TriggerSchedule, triggerName: "t", firedAt: t0}
	logName := logTriggerEvent("alpha", ev)
	sched := newScheduler()
	insts := []*fleet.Instance{
		{Name: "k-failed", Status: fleet.StatusFailed, Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "k1", Error: "kubernetes:image_pull: no such image"},
		{Name: "k-interrupted", Status: fleet.StatusFailed, Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "k2", Error: "kubernetes:interrupted: interrupted by daemon restart during create (at step seed); it was not resumed: rebuild it to retry, or destroy it"},
		{Name: "d-interrupted", Status: fleet.StatusFailed, Backend: fleet.BackendDevcontainer, Error: interruptedByRestart},
	}
	for _, inst := range insts {
		sched.watch(&watchedAgent{fleet: "alpha", instance: inst.Name, agent: "a-" + inst.Name, spawnedAt: t0, lastActive: t0, event: ev, eventLog: logName})
	}
	s.serviceWatched(context.Background(), sched, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: insts}}}, t0)
	if len(released) != 1 || released[0] != "k-failed" {
		t.Errorf("released %v, want k-failed", released)
	}
	if len(rec.reaped) != 1 || rec.reaped[0] != "k-interrupted" {
		t.Errorf("destroyed %v, want k-interrupted", rec.reaped)
	}
	if len(sched.watched) != 0 {
		t.Errorf("still watched: %v", keysOf(sched.watched))
	}
	assertLogHas(t, triggerLog(t, "alpha", "t"),
		"a-k-failed: create of k-failed failed: kubernetes:image_pull: no such image; the record is kept, its pod and disk are released",
		"a-k-interrupted: create of k-interrupted was interrupted by a daemon restart; it is being destroyed",
		"a-d-interrupted: create of d-interrupted was interrupted by a daemon restart; the record is kept",
	)
}

// Releasing a failed run lets its record go of its key and ContainerID
// first (so the quotas stop counting it), keeps its status and reason, and
// tears the instance down by the key it had. A second release, or one of a
// record that moved on, does nothing.
func TestReleaseFailedRunKeepsTheRecord(t *testing.T) {
	isolateFleetDir(t)
	inst := &fleet.Instance{Name: "run-1", Status: fleet.StatusFailed, Automated: true, Backend: fleet.BackendKubernetes,
		KubeCluster: "c1-abc234", KubeKey: "0123456789", ContainerID: "k8s:c1/ns/sts@g1", Error: "kubernetes:quota: full"}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}}}}); err != nil {
		t.Fatal(err)
	}
	downed := make(chan *fleet.Instance, 2)
	orig := jobDownInstance
	jobDownInstance = func(i *fleet.Instance) error { downed <- i; return nil }
	t.Cleanup(func() { jobDownInstance = orig })

	s := newService()
	releaseFailedRun(s, "alpha", inst)
	select {
	case got := <-downed:
		if got.KubeKey != "0123456789" || got.ContainerID != "k8s:c1/ns/sts@g1" {
			t.Fatalf("torn down %+v, want the run's key and container", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was never torn down")
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := findInstance(st, "alpha", "run-1")
	if got == nil || got.Status != fleet.StatusFailed || got.Error != "kubernetes:quota: full" || got.KubeKey != "" || got.ContainerID != "" || got.KubeCluster != "c1-abc234" {
		t.Fatalf("record after release = %+v", got)
	}
	releaseFailedRun(s, "alpha", inst)
	select {
	case again := <-downed:
		t.Fatalf("a released run was torn down again: %+v", again)
	case <-time.After(100 * time.Millisecond):
	}
}

// Queued runs survive a restart with their payloads; one that waited past
// automationQueueTTL expires instead, and its log says so. The slots of
// the restored runs are held again.
func TestRestoreRequeuesQueuedRuns(t *testing.T) {
	isolateFleetDir(t)
	t0 := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	st := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "k-1", Status: fleet.StatusRunning}}}}}
	stubScheduleState(t, st)
	fresh := &triggerEvent{kind: fleet.TriggerWebhook, triggerName: "hook", firedAt: t0, webhookName: "gh", body: []byte(`{"n":1}`)}
	stale := &triggerEvent{kind: fleet.TriggerWebhook, triggerName: "hook", firedAt: t0.Add(-2 * time.Hour), webhookName: "gh", body: []byte(`{"n":0}`)}
	freshLog, staleLog := logTriggerEvent("alpha", fresh), logTriggerEvent("alpha", stale)

	sched := persistedScheduler()
	sched.watch(&watchedAgent{fleet: "alpha", instance: "k-1", agent: "k", spawnedAt: t0, lastActive: t0, launched: true})
	sched.lim.restoreQueued([]*queuedRun{
		{id: 5, firing: 4, fleet: "alpha", trigger: "hook", agent: "k", prompt: "go", cluster: "c1", queuedAt: t0, why: "busy", event: fresh, eventLog: freshLog},
		{id: 2, firing: 1, fleet: "alpha", trigger: "hook", agent: "k", cluster: "c1", queuedAt: stale.firedAt, why: "busy", event: stale, eventLog: staleLog},
	})
	sched.persist()
	if wf := readPersistedWatch(t); len(wf.Queued) != 2 || wf.Queued[0].ID != 2 {
		t.Fatalf("persisted queue = %+v", wf.Queued)
	}

	next := persistedScheduler()
	next.restore(time.Now())
	got := next.lim.queuedRuns()
	if len(got) != 1 || got[0].id != 5 || got[0].prompt != "go" || string(got[0].event.body) != `{"n":1}` || got[0].eventLog != freshLog {
		t.Fatalf("restored queue = %+v", got)
	}
	if _, running := next.lim.counts(); running[agentKey("alpha", "k")] != 1 {
		t.Fatalf("the restored run holds no slot: %v", running)
	}
	assertLogHas(t, triggerLog(t, "alpha", "hook"), "k: expired after 1h0m0s in the queue; it waited because busy")
}

// At start the scheduler takes up the failed cluster runs that still hold
// something (a key, or an interrupted create), even ones watch.json lost,
// and leaves released ones and other backends' failures alone.
func TestRestoreTakesUpFailedClusterRuns(t *testing.T) {
	isolateFleetDir(t)
	k8s := func(name, key, errMsg string) *fleet.Instance {
		return &fleet.Instance{Name: name, Automated: true, Status: fleet.StatusFailed, Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: key, Error: errMsg}
	}
	stubScheduleState(t, &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
		k8s("holds", "k1", "kubernetes:image_pull: x"),
		k8s("interrupted", "k2", "kubernetes:interrupted: x"),
		k8s("released", "", "kubernetes:image_pull: x"),
		{Name: "docker", Automated: true, Status: fleet.StatusFailed, Backend: fleet.BackendDevcontainer, Error: "x"},
	}}}})
	sched := persistedScheduler()
	sched.restore(time.Now())
	got := keysOf(sched.watched)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alpha/holds" || got[1] != "alpha/interrupted" {
		t.Fatalf("taken up %v, want holds and interrupted", got)
	}
}
