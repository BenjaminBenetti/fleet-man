package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeChecks answers the limiter's questions from tables: each connection's
// limits, a max_instances per connection (counting records with a key, as
// quotaRoom does), and the agents the login check refuses.
type fakeChecks struct {
	limits   map[string]clusterLimits
	maxInsts map[string]int
	noLogin  map[string]bool
}

func (f *fakeChecks) checks() runChecks {
	return runChecks{
		limits: func(id string) (clusterLimits, bool) {
			l, ok := f.limits[id]
			return l, ok
		},
		quota: func(st *state.State, id string, extra int) error {
			max := f.maxInsts[id]
			if max == 0 {
				return nil
			}
			used := extra
			for _, fl := range st.Fleets {
				for _, inst := range fl.Instances {
					if inst.KubeCluster == id && inst.KubeKey != "" {
						used++
					}
				}
			}
			if used >= max {
				return status.Errorf(codes.ResourceExhausted, "kubernetes:quota: connection %q allows %d instances and has %d", id, max, used)
			}
			return nil
		},
		login: func(_ *state.State, _ string, ag fleet.Agent, name string) string {
			if f.noLogin[ag.Name] {
				return "log in to Claude once for this fleet on ☸ " + name
			}
			return ""
		},
	}
}

func clusterAgent(name, cluster string) fleet.Agent {
	return fleet.Agent{Name: name, Command: "claude", Backend: fleet.BackendKubernetes, KubeCluster: cluster}
}

// limitState is fleet alpha with agents and instances.
func limitState(agents []fleet.Agent, insts ...*fleet.Instance) *state.State {
	return &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{Agents: agents}, Instances: insts},
	}}
}

func planOne(l *automationLimiter, c runChecks, st *state.State, trigger fleet.Trigger, now time.Time) *firingPlan {
	p := l.plan(c, func() *state.State { return st }, []firingItem{{fleet: "alpha", trigger: trigger}}, now)[0]
	l.commit(p, &triggerEvent{triggerName: trigger.Name, firedAt: now}, "event-x.log")
	return p
}

func runNames(runs []*queuedRun) []string {
	var out []string
	for _, r := range runs {
		out = append(out, fmt.Sprintf("%s#%d", r.agent, r.firing))
	}
	return out
}

// Agents on the other backends fire at once, whatever a cluster's limits.
func TestLimiterFiresOtherBackendsAtOnce(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{}).checks()
	st := limitState([]fleet.Agent{{Name: "d", Backend: fleet.BackendDevcontainer}, {Name: "old"}})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"d", "old"}}
	for range 3 {
		p := planOne(l, c, st, trig, time.Now())
		if len(p.direct) != 2 || len(p.start)+len(p.queued)+len(p.refused) != 0 || !p.fired() {
			t.Fatalf("plan = %+v, want both agents direct", p)
		}
	}
}

// A trigger's firings on a cluster start one per fire interval; the rest
// wait in order, and the drain says when the next one may start.
func TestLimiterSpacesATriggersFiringsByTheInterval(t *testing.T) {
	l := newAutomationLimiter()
	f := &fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", interval: 30 * time.Second}}}
	c := f.checks()
	st := limitState([]fleet.Agent{clusterAgent("k", "c1")})
	trig := fleet.Trigger{Name: "burst", AgentNames: []string{"k"}}
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	load := func() *state.State { return st }

	first := planOne(l, c, st, trig, t0)
	if len(first.start) != 1 || !first.fired() {
		t.Fatalf("first firing = %+v, want it started", first)
	}
	l.started(first.start[0], "alpha/k-1")
	var queued []*firingPlan
	for i := 1; i <= 3; i++ {
		p := planOne(l, c, st, trig, t0.Add(time.Duration(i)*time.Second))
		if len(p.queued) != 1 || p.fired() {
			t.Fatalf("firing %d = %+v, want it queued", i, p)
		}
		queued = append(queued, p)
	}
	if why := queued[0].queued[0].why; !strings.Contains(why, "automation_fire_interval") || !strings.Contains(why, "☸ prod") {
		t.Errorf("first wait = %q", why)
	}
	if why := queued[1].queued[0].why; !strings.Contains(why, "an earlier firing of this trigger waits for agent k") {
		t.Errorf("second wait = %q", why)
	}

	d := l.due(c, load, t0.Add(10*time.Second))
	if len(d.start) != 0 || !d.next.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("drain at +10s = %v next %v, want none until +30s", runNames(d.start), d.next)
	}
	d = l.due(c, load, t0.Add(30*time.Second))
	if got := runNames(d.start); len(got) != 1 || got[0] != fmt.Sprintf("k#%d", queued[0].firing) {
		t.Fatalf("drain at +30s started %v, want the first queued firing", got)
	}
	l.started(d.start[0], "alpha/k-2")
	if d.next != t0.Add(60*time.Second) {
		t.Fatalf("next = %v, want +60s", d.next)
	}
	d = l.due(c, load, t0.Add(60*time.Second))
	if got := runNames(d.start); len(got) != 1 || got[0] != fmt.Sprintf("k#%d", queued[1].firing) {
		t.Fatalf("drain at +60s started %v, want the second queued firing", got)
	}
}

// An agent has at most automation_max_per_agent runs on its cluster; a
// slot given back lets the next queued run start.
func TestLimiterCapsAnAgentsRuns(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", maxPerAgent: 2}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("k", "c1")})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"k"}}
	now := time.Now()
	for i := range 2 {
		p := planOne(l, c, st, trig, now)
		if len(p.start) != 1 {
			t.Fatalf("firing %d = %+v, want a start", i, p)
		}
		l.started(p.start[0], fmt.Sprintf("alpha/k-%d", i))
	}
	p := planOne(l, c, st, trig, now)
	if len(p.queued) != 1 || !strings.Contains(p.queued[0].why, "agent k has 2 runs on ☸ prod, its limit (automation_max_per_agent)") {
		t.Fatalf("third firing = %+v", p.queued)
	}
	if d := l.due(c, func() *state.State { return st }, now); len(d.start) != 0 {
		t.Fatalf("started %v with both slots taken", runNames(d.start))
	}
	l.release("alpha/k-0")
	if d := l.due(c, func() *state.State { return st }, now); len(d.start) != 1 {
		t.Fatalf("started %v once a slot was free, want one", runNames(d.start))
	}
	queued, running := l.counts()
	if queued[agentKey("alpha", "k")] != 0 || running[agentKey("alpha", "k")] != 2 {
		t.Fatalf("counts = %v %v", queued, running)
	}
}

// A run the connection's quotas have no room for waits, with the quota's
// reason, and the runs reserved but not created yet count against them.
func TestLimiterQueuesOnTheQuotas(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod"}}, maxInsts: map[string]int{"c1": 2}}).checks()
	agents := []fleet.Agent{clusterAgent("a", "c1"), clusterAgent("b", "c1"), clusterAgent("x", "c1")}
	st := limitState(agents, &fleet.Instance{Name: "manual", KubeCluster: "c1", KubeKey: "k1"},
		// A failed run whose pod and disk were released holds no key.
		&fleet.Instance{Name: "released", KubeCluster: "c1", Status: fleet.StatusFailed})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"a", "b", "x"}}
	p := planOne(l, c, st, trig, time.Now())
	if len(p.start) != 1 || len(p.queued) != 2 {
		t.Fatalf("plan = start %v queued %v, want one of three to fit", runNames(p.start), runNames(p.queued))
	}
	if !strings.Contains(p.queued[0].why, "kubernetes:quota") {
		t.Fatalf("wait = %q", p.queued[0].why)
	}
	// Not created yet: the reservation still takes the room.
	if d := l.due(c, func() *state.State { return st }, time.Now()); len(d.start) != 0 {
		t.Fatalf("started %v over the quota", runNames(d.start))
	}
	l.started(p.start[0], "") // its create failed: the room is back
	d := l.due(c, func() *state.State { return st }, time.Now())
	if got := runNames(d.start); len(got) != 1 || !strings.HasPrefix(got[0], "b#") {
		t.Fatalf("started %v, want b (firing order)", got)
	}
}

// Runs of one firing on one cluster start together: the interval spaces
// firings, not the agents of one.
func TestLimiterStartsAFiringsAgentsTogether(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", interval: time.Minute}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("a", "c1"), clusterAgent("b", "c1")})
	p := planOne(l, c, st, fleet.Trigger{Name: "t", AgentNames: []string{"a", "b"}}, time.Now())
	if len(p.start) != 2 {
		t.Fatalf("plan = %+v, want both agents started", p)
	}
}

// One agent at its limit does not hold up the trigger's other agent.
func TestLimiterQueuesPerAgent(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", maxPerAgent: 1}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("a", "c1"), clusterAgent("b", "c1")})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"a", "b"}}
	now := time.Now()
	p := planOne(l, c, st, trig, now)
	l.started(p.start[0], "alpha/a-1")
	l.started(p.start[1], "alpha/b-1")
	p = planOne(l, c, st, trig, now)
	if len(p.queued) != 2 {
		t.Fatalf("second firing = %+v, want both waiting", p)
	}
	l.release("alpha/b-1")
	d := l.due(c, func() *state.State { return st }, now)
	if got := runNames(d.start); len(got) != 1 || !strings.HasPrefix(got[0], "b#") {
		t.Fatalf("started %v, want b alone", got)
	}
}

// A firing that would wait in a full queue is dropped whole, reserving
// nothing, with when to retry.
func TestLimiterDropsAFiringOnAFullQueue(t *testing.T) {
	orig := automationQueueMax
	automationQueueMax = 2
	t.Cleanup(func() { automationQueueMax = orig })
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", interval: time.Hour}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("k", "c1"), {Name: "d"}})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"k", "d"}}
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	for i := range 3 {
		if p := planOne(l, c, st, trig, t0); p.dropped != "" {
			t.Fatalf("firing %d dropped: %s", i, p.dropped)
		}
	}
	p := planOne(l, c, st, trig, t0.Add(10*time.Minute))
	if p.dropped == "" || len(p.direct)+len(p.start)+len(p.queued) != 0 {
		t.Fatalf("plan = %+v, want it dropped whole", p)
	}
	if !strings.Contains(p.dropped, "queue is full (2 firings") || p.retryAfter != 50*time.Minute {
		t.Fatalf("dropped %q, retry after %v", p.dropped, p.retryAfter)
	}
	if q, _ := l.counts(); q[agentKey("alpha", "k")] != 2 {
		t.Fatalf("queued = %v, want the two earlier firings only", q)
	}
}

// A delivery that never reached the scheduler gives its reservations back:
// its queued runs leave the queue, its started ones their slot and the
// trigger's start mark.
func TestLimiterDiscardUndoesAPlan(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", interval: time.Minute, maxPerAgent: 1}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("k", "c1")})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"k"}}
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	kept := planOne(l, c, st, trig, t0)
	l.started(kept.start[0], "alpha/k-1")
	l.release("alpha/k-1")

	load := func() *state.State { return st }
	plans := l.plan(c, load, []firingItem{{fleet: "alpha", trigger: trig}, {fleet: "alpha", trigger: trig}}, t0.Add(2*time.Minute))
	if len(plans[0].start) != 1 || len(plans[1].queued) != 1 {
		t.Fatalf("plans = %+v %+v", plans[0], plans[1])
	}
	l.discard(plans)
	if q, r := l.counts(); len(q) != 0 || len(r) != 0 || len(l.creating) != 0 {
		t.Fatalf("after discard: queued %v running %v creating %v", q, r, l.creating)
	}
	if m := l.starts[startKey("alpha", "t", "c1")]; !m.at.Equal(t0) || m.firing != kept.firing {
		t.Fatalf("start mark = %+v, want the first firing's back", m)
	}
}

// A queued run planned for a delivery still on its way stays out of the
// drain until committed, and holds up later runs of its agent.
func TestLimiterLeavesPendingRunsAlone(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", maxPerAgent: 1}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("k", "c1")})
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"k"}}
	load := func() *state.State { return st }
	now := time.Now()
	first := planOne(l, c, st, trig, now)
	l.started(first.start[0], "alpha/k-1")
	pending := l.plan(c, load, []firingItem{{fleet: "alpha", trigger: trig}}, now)[0]
	l.release("alpha/k-1")
	if d := l.due(c, load, now); len(d.start) != 0 {
		t.Fatalf("drained %v before commit", runNames(d.start))
	}
	l.commit(pending, &triggerEvent{}, "event-y.log")
	if d := l.due(c, load, now); len(d.start) != 1 || d.start[0].eventLog != "event-y.log" {
		t.Fatalf("after commit drained %v", runNames(d.start))
	}
}

// The drain refuses a queued run whose agent, connection or login is gone
// now, starts one whose agent moved off its cluster at once, and expires
// one past automationQueueTTL.
func TestLimiterDrainRefusesAndExpires(t *testing.T) {
	l := newAutomationLimiter()
	f := &fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", maxPerAgent: 1}, "c2": {name: "dev", maxPerAgent: 1}}, noLogin: map[string]bool{}}
	c := f.checks()
	agents := []fleet.Agent{clusterAgent("gone", "c1"), clusterAgent("forgot", "c2"), clusterAgent("login", "c1"), clusterAgent("moved", "c1"), clusterAgent("old", "c1")}
	st := limitState(agents)
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	load := func() *state.State { return st }
	for _, ag := range agents {
		trig := fleet.Trigger{Name: "t-" + ag.Name, AgentNames: []string{ag.Name}}
		at := t0.Add(30 * time.Minute)
		if ag.Name == "old" {
			at = t0
		}
		p := planOne(l, c, st, trig, at)
		l.started(p.start[0], "alpha/"+ag.Name+"-1")
		planOne(l, c, st, trig, at) // waits for the slot
	}
	st.Fleets["alpha"].Settings.Agents = []fleet.Agent{
		clusterAgent("forgot", "c2"), clusterAgent("login", "c1"),
		{Name: "moved", Backend: fleet.BackendDevcontainer}, clusterAgent("old", "c1"),
	}
	delete(f.limits, "c2")
	f.noLogin["login"] = true
	d := l.due(c, load, t0.Add(time.Hour))
	why := map[string]string{}
	for _, r := range d.refused {
		why[r.agent] = r.why
	}
	if why["gone"] != "agent gone no longer exists" || why["forgot"] != "cluster c2 is no longer connected" || why["login"] != "log in to Claude once for this fleet on ☸ prod" {
		t.Errorf("refused %v", why)
	}
	if got := runNames(d.start); len(got) != 1 || !strings.HasPrefix(got[0], "moved#") || d.start[0].def.Backend != fleet.BackendDevcontainer {
		t.Errorf("started %v, want the run whose agent moved to devcontainer", got)
	}
	if got := runNames(d.expired); len(got) != 1 || !strings.HasPrefix(got[0], "old#") {
		t.Errorf("expired %v, want old's", got)
	}
}

// The queues survive a restart: the runs a snapshot lists come back in
// order, and new firings number after them.
func TestLimiterRestoresQueuedRuns(t *testing.T) {
	l := newAutomationLimiter()
	l.restoreQueued([]*queuedRun{
		{id: 9, firing: 8, fleet: "alpha", trigger: "t", agent: "k", cluster: "c1", queuedAt: time.Now()},
		{id: 4, firing: 3, fleet: "alpha", trigger: "t", agent: "k", cluster: "c1", queuedAt: time.Now()},
	})
	got := l.queuedRuns()
	if len(got) != 2 || got[0].id != 4 || got[1].id != 9 {
		t.Fatalf("queued = %v", runNames(got))
	}
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod"}}}).checks()
	p := planOne(l, c, limitState([]fleet.Agent{clusterAgent("k", "c1")}), fleet.Trigger{Name: "t", AgentNames: []string{"k"}}, time.Now())
	if p.firing <= 9 || len(p.queued) != 1 {
		t.Fatalf("new firing %d after the restored ones = %+v", p.firing, p)
	}
}

// The webhook receiver and the scheduler use the limiter at once: plans,
// discards, drains and settles from many goroutines leave its books
// consistent (every slot and create given back), and -race finds nothing.
func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	l := newAutomationLimiter()
	c := (&fakeChecks{limits: map[string]clusterLimits{"c1": {name: "prod", maxPerAgent: 3}}}).checks()
	st := limitState([]fleet.Agent{clusterAgent("k", "c1")})
	load := func() *state.State { return st }
	trig := fleet.Trigger{Name: "t", AgentNames: []string{"k"}}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				p := l.plan(c, load, []firingItem{{fleet: "alpha", trigger: trig}}, time.Now())[0]
				if (g+i)%3 == 0 {
					l.discard([]*firingPlan{p})
					continue
				}
				l.commit(p, &triggerEvent{}, "")
				for _, r := range p.start {
					l.started(r, "")
				}
				for _, r := range l.due(c, load, time.Now()).start {
					l.started(r, "")
				}
				l.counts()
				l.queuedRuns()
			}
		}()
	}
	wg.Wait()
	for {
		d := l.due(c, load, time.Now())
		if len(d.start) == 0 {
			break
		}
		for _, r := range d.start {
			l.started(r, "")
		}
	}
	if q, r := l.counts(); len(q) != 0 || len(r) != 0 || len(l.creating) != 0 || len(l.holder) != 0 {
		t.Fatalf("books after the storm: queued %v running %v creating %v holders %v", q, r, l.creating, l.holder)
	}
}
