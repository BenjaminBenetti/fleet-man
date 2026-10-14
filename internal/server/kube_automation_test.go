//go:build !nok8s

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const autoCluster = "itest-abc234"

// seedAutomationCluster saves connection autoCluster ("itest") with the
// given automation limits, and fleet alpha with agents and triggers.
func seedAutomationCluster(t *testing.T, interval string, maxPerAgent int, agents []fleet.Agent, triggers []fleet.Trigger) *state.State {
	t.Helper()
	isolateFleetDir(t)
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{{ID: autoCluster, DisplayName: "itest", Context: "minikube", Namespace: "fleet",
			AutomationFireInterval: interval, AutomationMaxPerAgent: &maxPerAgent}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git", Settings: fleet.FleetSettings{Agents: agents, Triggers: triggers}},
	}}
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}
	return st
}

// keyedAgent is a cluster agent whose command signs claude in with a key,
// so the login check lets it run.
func keyedAgent(name, cluster string) fleet.Agent {
	return fleet.Agent{Name: name, Command: "ANTHROPIC_API_KEY=x claude '${PROMPT}'", Backend: fleet.BackendKubernetes, KubeCluster: cluster}
}

// stubClusterCreates makes createAutomationInstance add a CREATING record
// with a key to st (as startCreateInstanceJob does) and records the names.
func stubClusterCreates(t *testing.T, st *state.State) *[]string {
	t.Helper()
	var created []string
	orig := createAutomationInstance
	createAutomationInstance = func(_ *service, fleetName string, ag fleet.Agent, now time.Time) (string, error) {
		name := automationInstanceName(ag.Name, now)
		st.Fleets[fleetName].Instances = append(st.Fleets[fleetName].Instances, &fleet.Instance{
			Name: name, Status: fleet.StatusCreating, Backend: ag.Backend, KubeCluster: ag.KubeCluster, KubeKey: "k-" + name, CreatedAt: now,
		})
		created = append(created, name)
		return name, nil
	}
	t.Cleanup(func() { createAutomationInstance = orig })
	return &created
}

func settingsProto(agents ...fleet.Agent) *fleetgrpc.FleetSettings {
	return protoconv.FleetSettingsToProto(fleet.FleetSettings{Agents: agents})
}

// A webhook delivery's reply says what became of it: 200 when its trigger
// fired, 202 when its run waits for the cluster's fire interval, and 429
// with Retry-After (enqueueing nothing) once the trigger's queue is full. A
// delivery the scheduler cannot take gives its reservations back.
func TestWebhookRepliesSayWhatBecameOfTheDelivery(t *testing.T) {
	orig := automationQueueMax
	automationQueueMax = 2
	t.Cleanup(func() { automationQueueMax = orig })
	trig := fleet.Trigger{Name: "burst", Type: fleet.TriggerWebhook, AgentNames: []string{"k"}, WebhookName: "gh", FilterType: fleet.WebhookFilterRegex, Regex: ".*"}
	st := seedAutomationCluster(t, "1h", 0, []fleet.Agent{keyedAgent("k", autoCluster)}, []fleet.Trigger{trig})
	stubScheduleState(t, st)
	stubAutomationSeams(t)
	created := stubClusterCreates(t, st)
	s := newService()
	sched := newScheduler()
	sched.lim = s.limiter()

	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.serveWebhook(w, httptest.NewRequest(http.MethodPost, "/gh", strings.NewReader(`{"n":1}`)))
		return w
	}
	take := func() {
		t.Helper()
		select {
		case b := <-s.triggerFires:
			s.fireTriggerBatch(sched, b, time.Now())
		default:
			t.Fatal("the delivery enqueued nothing")
		}
	}

	if w := post(); w.Code != http.StatusOK || w.Body.String() != "received: 1 trigger(s) fired\n" {
		t.Fatalf("first delivery = %d %q", w.Code, w.Body.String())
	}
	take()
	if len(*created) != 1 {
		t.Fatalf("created %v", *created)
	}
	for i := range 2 {
		if w := post(); w.Code != http.StatusAccepted || w.Body.String() != "received: 0 fired, 1 queued\n" {
			t.Fatalf("delivery %d = %d %q", i+2, w.Code, w.Body.String())
		}
		take()
	}
	w := post()
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "burst") {
		t.Fatalf("delivery over a full queue = %d %q", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("a 429 enqueued %+v", b)
	}
	logs := triggerLog(t, "alpha", "burst")
	if n := strings.Count(logs, "===== event-"); n != 3 {
		t.Fatalf("%d firings logged, want 3 (the 429 is the sender's to see):\n%s", n, logs)
	}
	assertLogHas(t, logs, "k: instance "+(*created)[0]+" (☸ itest)", "k: queued: the trigger started a run on ☸ itest")

	// A delivery the scheduler cannot take (no room in its channel) is shed
	// with a 503 and leaves nothing reserved.
	automationQueueMax = 3
	s.triggerFires = make(chan []triggerFire)
	before, _ := s.limiter().counts()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.serveWebhook(rec, httptest.NewRequest(http.MethodPost, "/gh", strings.NewReader("{}")).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shed delivery = %d", rec.Code)
	}
	if after, _ := s.limiter().counts(); after[agentKey("alpha", "k")] != before[agentKey("alpha", "k")] {
		t.Fatalf("a shed delivery left runs queued: %v -> %v", before, after)
	}
}

// A delivery the sender was told fired, but that the scheduler had not
// taken when it stopped, is not lost: its firing is logged, its runs (a
// cluster agent's and a devcontainer agent's) wait in the trigger's queue,
// and the next daemon starts them with the delivery's payload. A delivery
// after the stop is refused (503), for the sender to retry.
func TestStoppingSchedulerKeepsTheFiresItHadNotTaken(t *testing.T) {
	trig := fleet.Trigger{Name: "hook", Type: fleet.TriggerWebhook, AgentNames: []string{"k", "d"}, WebhookName: "gh",
		FilterType: fleet.WebhookFilterRegex, Regex: ".*", Prompt: "P"}
	direct := fleet.Agent{Name: "d", Command: "true"}
	st := seedAutomationCluster(t, "0", 0, []fleet.Agent{keyedAgent("k", autoCluster), direct}, []fleet.Trigger{trig})
	stubScheduleState(t, st)
	stubAutomationSeams(t)
	created := stubClusterCreates(t, st)
	s := newService()
	sched := persistedScheduler()
	sched.lim = s.limiter()
	t0 := time.Now().Truncate(time.Second)

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.serveWebhook(w, httptest.NewRequest(http.MethodPost, "/gh", strings.NewReader(body)))
		return w
	}
	if w := post(`{"n":1}`); w.Code != http.StatusOK {
		t.Fatalf("delivery = %d %q", w.Code, w.Body.String())
	}
	s.holdFires(sched, s.closeFires(), t0)
	if len(*created) != 0 {
		t.Fatalf("a stopping scheduler created %v", *created)
	}
	if w := post(`{"n":2}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("delivery after the stop = %d %q", w.Code, w.Body.String())
	}
	logs := triggerLog(t, "alpha", "hook")
	if n := strings.Count(logs, "===== event-"); n != 1 {
		t.Fatalf("%d firings logged, want the one delivered before the stop:\n%s", n, logs)
	}
	why := "queued: the daemon stopped before it started the run; the next one starts it"
	assertLogHas(t, logs, "k: "+why, "d: "+why)
	if q := readPersistedWatch(t).Queued; len(q) != 2 {
		t.Fatalf("watch.json keeps %+v, want both runs queued", q)
	}

	// The next daemon.
	s2 := newService()
	next := persistedScheduler()
	next.lim = s2.limiter()
	next.restore(t0.Add(time.Second))
	s2.schedulerTick(context.Background(), next, t0.Add(time.Second))
	if len(*created) != 2 {
		t.Fatalf("the next daemon created %v, want both runs", *created)
	}
	for _, name := range *created {
		w := next.watched["alpha/"+name]
		if w == nil || w.prompt != "P" || w.event == nil || string(w.event.body) != `{"n":1}` {
			t.Fatalf("run %s started as %+v", name, w)
		}
	}
}

// The scheduler loop stopping with fires in its buffer keeps all of them,
// though its select sees the stop and a ready batch at once and may take
// either: none starts a run, every one waits in the queue on disk.
func TestSchedulerLoopStopsWithFiresInItsBuffer(t *testing.T) {
	trig := fleet.Trigger{Name: "hook", Type: fleet.TriggerWebhook, AgentNames: []string{"d"}, WebhookName: "gh"}
	for round := range 10 {
		t.Run(strconv.Itoa(round), func(t *testing.T) {
			st := seedAutomationCluster(t, "0", 0, []fleet.Agent{{Name: "d", Command: "true"}}, []fleet.Trigger{trig})
			stubScheduleState(t, st)
			stubAutomationSeams(t)
			created := stubClusterCreates(t, st)
			s := newService()
			s.hub = nil // no status to publish
			for n := range 5 {
				if !s.handFires(context.Background(), []triggerFire{{fleet: "alpha", trigger: trig, body: []byte(strconv.Itoa(n))}}, time.Second) {
					t.Fatalf("fire %d was not handed over", n)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.runScheduler(ctx)
			if len(*created) != 0 {
				t.Fatalf("a stopping scheduler created %v", *created)
			}
			if q := readPersistedWatch(t).Queued; len(q) != 5 {
				t.Fatalf("watch.json keeps %d runs, want the 5 fires': %+v", len(q), q)
			}
		})
	}
}

// An agent on a forgotten connection, or one whose tool was never logged in
// on its cluster, is refused with the reason in its trigger log; a login in
// the location's store, an API key in the devcontainer env, or the check
// turned off lets it run.
func TestClusterAgentRefusals(t *testing.T) {
	claude := fleet.Agent{Name: "c", Command: fleet.DefaultAgentCommand, Backend: fleet.BackendKubernetes, KubeCluster: autoCluster}
	gone := fleet.Agent{Name: "g", Command: "true", Backend: fleet.BackendKubernetes, KubeCluster: "forgotten-xyz234"}
	trig := fleet.Trigger{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"c", "g"}}
	st := seedAutomationCluster(t, "0", 0, []fleet.Agent{claude, gone}, []fleet.Trigger{trig})
	stubScheduleState(t, st)
	stubAutomationSeams(t)
	created := stubClusterCreates(t, st)
	s := newService()
	sched := newScheduler()
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

	s.fireTriggerAgents(sched, "alpha", trig, []fleet.Agent{claude, gone}, t0, nil)
	if len(*created) != 0 {
		t.Fatalf("created %v", *created)
	}
	assertLogHas(t, triggerLog(t, "alpha", "t"),
		"c: refused: log in to Claude once for this fleet on ☸ itest",
		"g: refused: cluster forgotten-xyz234 is no longer connected",
	)

	// The checkout's devcontainer env sets the key.
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".devcontainer", "devcontainer.json"), []byte(`{
		// the key comes from the daemon host
		"remoteEnv": {"ANTHROPIC_API_KEY": "${localEnv:ANTHROPIC_API_KEY}"},
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st.Fleets["alpha"].Instances = []*fleet.Instance{{Name: "manual", WorkspaceDir: ws, Status: fleet.StatusStopped}}
	// Passed through from a variable the daemon host does not have, the key
	// is empty in the pod: still no login.
	t.Setenv("ANTHROPIC_API_KEY", "")
	s.fireTriggerAgents(sched, "alpha", trig, []fleet.Agent{claude}, t0.Add(30*time.Second), nil)
	if len(*created) != 0 {
		t.Fatalf("with the key's variable unset on the daemon host created %v", *created)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	s.fireTriggerAgents(sched, "alpha", trig, []fleet.Agent{claude}, t0.Add(time.Minute), nil)
	if len(*created) != 1 {
		t.Fatalf("with the key in the devcontainer env created %v", *created)
	}

	// A login in the location's store.
	st.Fleets["alpha"].Instances = nil
	loginFile := filepath.Join(state.SyncLocationDir("alpha", autoCluster), ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(loginFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loginFile, []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.fireTriggerAgents(sched, "alpha", trig, []fleet.Agent{claude}, t0.Add(2*time.Minute), nil)
	if len(*created) != 2 {
		t.Fatalf("logged in at the location: created %v", *created)
	}

	// A logout there (the file gone), with the check turned off.
	if err := os.Remove(loginFile); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAutomationLoginCheck, "off")
	s.fireTriggerAgents(sched, "alpha", trig, []fleet.Agent{claude}, t0.Add(3*time.Minute), nil)
	if len(*created) != 3 {
		t.Fatalf("check off: created %v", *created)
	}
}

// A queued cluster run starts once the cluster's fire interval has passed,
// and the tick says when that is.
func TestQueuedClusterRunStartsAfterTheInterval(t *testing.T) {
	trig := fleet.Trigger{Name: "hook", Type: fleet.TriggerWebhook, AgentNames: []string{"k"}, WebhookName: "gh"}
	st := seedAutomationCluster(t, "30s", 0, []fleet.Agent{keyedAgent("k", autoCluster)}, []fleet.Trigger{trig})
	stubScheduleState(t, st)
	stubAutomationSeams(t)
	created := stubClusterCreates(t, st)
	s := newService()
	sched := newScheduler()
	t0 := time.Now().Truncate(time.Second)

	s.fireTriggerBatch(sched, []triggerFire{{fleet: "alpha", trigger: trig, body: []byte("1")}}, t0)
	s.fireTriggerBatch(sched, []triggerFire{{fleet: "alpha", trigger: trig, body: []byte("2")}}, t0.Add(5*time.Second))
	if len(*created) != 1 {
		t.Fatalf("created %v, want the first run only", *created)
	}
	if next := s.schedulerTick(context.Background(), sched, t0.Add(10*time.Second)); !next.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("next = %v, want %v", next, t0.Add(30*time.Second))
	}
	s.schedulerTick(context.Background(), sched, t0.Add(30*time.Second))
	if len(*created) != 2 {
		t.Fatalf("created %v once the interval passed", *created)
	}
	w := sched.watched["alpha/"+(*created)[1]]
	if w == nil || string(w.event.body) != "2" || w.command != keyedAgent("k", autoCluster).Command {
		t.Fatalf("the queued run started as %+v", w)
	}
}

// The automation status names the agents the login check refuses (with
// the tool to log in to) and every agent's queued and running runs.
func TestAutomationStatusNamesTheLoginAndTheRuns(t *testing.T) {
	refused := fleet.Agent{Name: "needs-login", Command: "codex exec '${PROMPT}'", Backend: fleet.BackendKubernetes, KubeCluster: autoCluster}
	keyed := keyedAgent("keyed", autoCluster)
	docker := fleet.Agent{Name: "docker", Command: "claude", Backend: fleet.BackendDevcontainer}
	st := seedAutomationCluster(t, "", 0, []fleet.Agent{refused, keyed, docker}, nil)
	s := newService()
	lim := newAutomationLimiter()
	lim.hold("alpha", "keyed", "alpha/keyed-1")
	lim.restoreQueued([]*queuedRun{{id: 1, firing: 1, fleet: "alpha", trigger: "t", agent: "keyed", queuedAt: time.Now()}})

	got := s.automationStatus(lim, st)
	if len(got.GetAgents()) != 2 {
		t.Fatalf("status = %v", got)
	}
	a, b := got.GetAgents()[0], got.GetAgents()[1]
	if a.GetAgent() != "keyed" || a.GetQueued() != 1 || a.GetRunning() != 1 || a.GetLogin() != "" {
		t.Errorf("keyed = %v", a)
	}
	if b.GetAgent() != "needs-login" || b.GetLogin() != "log in to Codex once for this fleet on ☸ itest" || b.GetLoginTool() != "codex" {
		t.Errorf("needs-login = %v", b)
	}
}

// Saving an agent the login check will refuse warns; saving it again
// unchanged, or one with its key in the command, does not.
func TestSetFleetSettingsWarnsOfARefusedLogin(t *testing.T) {
	seedAutomationCluster(t, "", 0, nil, nil)
	s := newService()
	claude := fleet.Agent{Name: "w", Command: fleet.DefaultAgentCommand, Backend: fleet.BackendKubernetes, KubeCluster: autoCluster}
	save := func(agents ...fleet.Agent) []string {
		t.Helper()
		reply, err := s.SetFleetSettings(context.Background(), &fleetgrpc.SetFleetSettingsRequest{Fleet: "alpha", Settings: settingsProto(agents...)})
		if err != nil {
			t.Fatal(err)
		}
		return reply.GetWarnings()
	}
	if got := save(claude); len(got) != 1 || got[0] != "agent w: log in to Claude once for this fleet on ☸ itest; until then its firings are refused" {
		t.Fatalf("warnings = %q", got)
	}
	if got := save(claude); len(got) != 0 {
		t.Fatalf("an unchanged agent warned again: %q", got)
	}
	if got := save(claude, keyedAgent("k", autoCluster)); len(got) != 0 {
		t.Fatalf("a keyed agent warned: %q", got)
	}
}

// The status reaches clients that do not watch: GetAutomationStatus (what
// `fleet agent list` shows, local or remote) for one fleet or all, and the
// MCP automation list's agents.
func TestGetAutomationStatusAndTheMCPList(t *testing.T) {
	refused := fleet.Agent{Name: "needs-login", Command: "codex exec '${PROMPT}'", Backend: fleet.BackendKubernetes, KubeCluster: autoCluster}
	keyed := keyedAgent("keyed", autoCluster)
	seedAutomationCluster(t, "", 0, []fleet.Agent{refused, keyed}, nil)
	s := newService()
	s.automation = newAutomationLimiter()
	s.automation.hold("alpha", "keyed", "alpha/keyed-1")
	s.automation.restoreQueued([]*queuedRun{{id: 1, firing: 1, fleet: "alpha", trigger: "t", agent: "keyed", queuedAt: time.Now()}})

	for _, name := range []string{"alpha", ""} {
		reply, err := s.GetAutomationStatus(context.Background(), &fleetgrpc.GetAutomationStatusRequest{Fleet: name})
		if err != nil {
			t.Fatal(err)
		}
		if agents := reply.GetStatus().GetAgents(); len(agents) != 2 || agents[0].GetAgent() != "keyed" || agents[0].GetQueued() != 1 || agents[1].GetLogin() == "" {
			t.Fatalf("fleet %q: status %v", name, reply.GetStatus())
		}
	}
	if _, err := s.GetAutomationStatus(context.Background(), &fleetgrpc.GetAutomationStatusRequest{Fleet: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("an unknown fleet = %v, want NotFound", err)
	}

	_, out, err := s.mcpAutomationList(context.Background(), nil, FleetAutomationListInput{Fleet: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]mcpAgent{}
	for _, a := range out.Agents {
		byName[a.Name] = a
	}
	if k := byName["keyed"]; k.Queued != 1 || k.Running != 1 || k.Login != "" {
		t.Errorf("keyed = %+v", k)
	}
	if n := byName["needs-login"]; n.Login != "log in to Codex once for this fleet on ☸ itest" {
		t.Errorf("needs-login = %+v", n)
	}
}

// A devcontainer env entry sets its variable when its value does once the
// devcontainer CLI resolves it on the daemon host.
func TestDevcontainerEnvSet(t *testing.T) {
	lookup := func(name string) (string, bool) {
		v, ok := map[string]string{"KEY": "sk-1", "EMPTY": ""}[name]
		return v, ok
	}
	for v, want := range map[any]bool{
		"sk-literal":                           true,
		"":                                     false,
		"${localEnv:KEY}":                      true,
		"${localEnv:MISSING}":                  false,
		"${localEnv:EMPTY}":                    false,
		"${localEnv:MISSING:fallback}":         true,
		"${localEnv:MISSING:}":                 false,
		"Bearer ${localEnv:MISSING}":           true,
		"${localEnv:MISSING}${localEnv:EMPTY}": false,
		"${containerEnv:KEY_IN_IMAGE}":         true,
		true:                                   true,
	} {
		if got := devcontainerEnvSet(v, lookup); got != want {
			t.Errorf("devcontainerEnvSet(%q) = %v, want %v", v, got, want)
		}
	}
	if devcontainerEnvSet(nil, lookup) {
		t.Error("a null value sets its variable")
	}
}
