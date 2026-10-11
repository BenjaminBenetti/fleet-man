package server

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// automation_limits.go keeps automation on a cluster affordable. A run of a
// Kubernetes agent costs a pod and a disk on someone's cluster, so the
// connection's record bounds it, next to its quotas:
//
//   - automation_fire_interval (30s by default): the shortest time between
//     two firings of one trigger that start runs there;
//   - automation_max_per_agent (4 by default): how many runs one agent may
//     have there at once;
//   - max_instances and max_total_disk, which every create checks.
//
// A run over a limit is not dropped: it waits in its trigger's queue (at
// most automationQueueMax firings, each kept automationQueueTTL) and starts
// in order as the limits allow, so a burst still gets one run per event,
// later. Agents on Docker, Coder and Codespaces fire at once, as always.
//
// The limiter is shared by the scheduler goroutine and the webhook receiver,
// so a delivery's reply can say what became of it (fired, queued, or 429
// when every matching queue is full): a firing is planned, and its runs
// reserved (to start now, or in the queue), before the reply is written.

// kubeGlyph marks a cluster in messages ("☸ prod-eu"), as the TUI does.
const kubeGlyph = "☸"

// Queue bounds; variables so tests can shorten them.
var (
	// automationQueueMax is how many firings one trigger's queue holds.
	automationQueueMax = 20
	// automationQueueTTL is how long a queued run waits before it expires.
	automationQueueTTL = time.Hour
)

// clusterLimits are a connection's limits on automation runs.
type clusterLimits struct {
	name        string        // the connection's display name
	interval    time.Duration // automation_fire_interval; 0 = off
	maxPerAgent int           // automation_max_per_agent; 0 = off
}

// runChecks is what the limiter asks the daemon about a cluster run.
type runChecks struct {
	// limits are connection id's; ok is false once it is gone.
	limits func(id string) (clusterLimits, bool)
	// quota is the connection's quota check with extra creates on top of
	// the records of st (kubeManager.quotaRoom).
	quota func(st *state.State, id string, extra int) error
	// login is the login check's refusal of ag in fleetName, run on the
	// connection named clusterName ("" when it may run).
	login func(st *state.State, fleetName string, ag fleet.Agent, clusterName string) string
}

// queuedRun is one cluster agent's run of a firing: reserved to start at
// once, or waiting in its trigger's queue.
type queuedRun struct {
	id     uint64
	firing uint64
	fleet  string
	// trigger and agent are names; prompt is the trigger's prompt when it
	// fired. The agent's command is read when the run starts.
	trigger string
	agent   string
	prompt  string
	// cluster is the agent's connection when the run was last looked at.
	cluster  string
	queuedAt time.Time
	// why it waits, for its outcome lines.
	why string
	// event is the firing (its payload rides it); eventLog its trigger log.
	event    *triggerEvent
	eventLog string
	// pending: planned for a webhook delivery whose batch has not reached
	// the scheduler yet; the queue's drain leaves it alone until then.
	pending bool
	// def is the agent as it was when the run was reserved to start.
	def fleet.Agent
}

// reservation is the holder key a run reserved to start holds its agent's
// slot under until its instance exists.
func (r *queuedRun) reservation() string { return fmt.Sprintf("#%d", r.id) }

// startMark is the last firing of a trigger that started a run on a
// connection, and when.
type startMark struct {
	at     time.Time
	firing uint64
}

// refusedRun is an agent run a firing does not start, and why.
type refusedRun struct {
	agent string
	why   string
}

// firingPlan is what one firing of a trigger does, decided before any of it
// happens.
type firingPlan struct {
	fleet   string
	trigger fleet.Trigger
	firing  uint64
	// direct are the agents on Docker, Coder or Codespaces: started at once.
	direct []fleet.Agent
	// start are the cluster runs reserved to start at once; queued the ones
	// waiting in the trigger's queue; refused the ones that never run.
	start   []*queuedRun
	queued  []*queuedRun
	refused []refusedRun
	// dropped says why nothing of the firing runs (the trigger's queue is
	// full); retryAfter is when a delivery may be worth sending again.
	dropped    string
	retryAfter time.Duration
	// marks are the start marks the plan's reservations replaced, restored
	// by a release.
	marks map[string]*startMark
}

// fired reports whether the plan starts or refuses all of its runs at once
// (a webhook reply's "fired").
func (p *firingPlan) fired() bool { return p.dropped == "" && len(p.queued) == 0 }

// automationLimiter holds the limits' bookkeeping and the queues.
type automationLimiter struct {
	mu  sync.Mutex
	seq uint64
	// queues are each trigger's waiting runs in firing order, by
	// firedKey(fleet, trigger).
	queues map[string][]*queuedRun
	// starts are the last start marks, by startKey.
	starts map[string]startMark
	// holds are the runs holding a slot of each agent (by agentKey): the
	// watched runs ("fleet/instance") and the runs reserved to start
	// (reservation()); holder maps each back to its agent.
	holds  map[string]map[string]bool
	holder map[string]string
	// creating counts each connection's runs reserved to start whose
	// records do not exist yet.
	creating map[string]int
}

func newAutomationLimiter() *automationLimiter {
	return &automationLimiter{
		queues:   map[string][]*queuedRun{},
		starts:   map[string]startMark{},
		holds:    map[string]map[string]bool{},
		holder:   map[string]string{},
		creating: map[string]int{},
	}
}

func agentKey(fleetName, agentName string) string { return fleetName + "\x00" + agentName }

func startKey(fleetName, triggerName, cluster string) string {
	return fleetName + "\x00" + triggerName + "\x00" + cluster
}

// hold has holder (a watched run, "fleet/instance") take a slot of the
// agent's. An adopted run, which has no agent, holds none.
func (l *automationLimiter) hold(fleetName, agentName, holder string) {
	if agentName == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holdLocked(agentKey(fleetName, agentName), holder)
}

func (l *automationLimiter) holdLocked(agent, holder string) {
	if l.holds[agent] == nil {
		l.holds[agent] = map[string]bool{}
	}
	l.holds[agent][holder] = true
	l.holder[holder] = agent
}

// release gives back the slot holder holds, if any.
func (l *automationLimiter) release(holder string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseLocked(holder)
}

func (l *automationLimiter) releaseLocked(holder string) {
	agent, ok := l.holder[holder]
	if !ok {
		return
	}
	delete(l.holder, holder)
	delete(l.holds[agent], holder)
	if len(l.holds[agent]) == 0 {
		delete(l.holds, agent)
	}
}

// firingItem is one firing to plan: a trigger of a fleet, with the agents
// it fires (nil: the trigger's agents as the state has them).
type firingItem struct {
	fleet   string
	trigger fleet.Trigger
	agents  []fleet.Agent
}

// plan plans each of items (planLocked) against the state load reads,
// which it reads under the limiter's lock: a run created before has its
// record in it, and one reserved but not created yet counts in creating, so
// the quotas count every run exactly once or more, never none.
func (l *automationLimiter) plan(c runChecks, load func() *state.State, items []firingItem, now time.Time) []*firingPlan {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := loadOrEmpty(load)
	plans := make([]*firingPlan, 0, len(items))
	for _, it := range items {
		agents := it.agents
		if agents == nil {
			agents = agentsForTrigger(st.Fleets[it.fleet], it.trigger)
		}
		plans = append(plans, l.planLocked(c, st, it.fleet, it.trigger, agents, now))
	}
	return plans
}

// loadOrEmpty is load's state, or an empty one when there is no state to
// read.
func loadOrEmpty(load func() *state.State) *state.State {
	if load != nil {
		if st := load(); st != nil {
			return st
		}
	}
	return &state.State{Fleets: map[string]*fleet.Fleet{}}
}

// planLocked decides the firing of trigger in fleetName with agents and
// reserves what it decided: the runs that start now hold their agent's
// slot and the trigger's start mark, the waiting ones go into the queue,
// pending until commit. Nothing is reserved for a firing dropped on a full
// queue.
func (l *automationLimiter) planLocked(c runChecks, st *state.State, fleetName string, trigger fleet.Trigger, agents []fleet.Agent, now time.Time) *firingPlan {
	l.seq++
	p := &firingPlan{fleet: fleetName, trigger: trigger, firing: l.seq}
	var runs []*queuedRun
	for _, ag := range agents {
		target := ag.DeployTarget()
		if target.Backend != fleet.BackendKubernetes {
			p.direct = append(p.direct, ag)
			continue
		}
		if why := refusal(c, st, fleetName, ag); why != "" {
			p.refused = append(p.refused, refusedRun{agent: ag.Name, why: why})
			continue
		}
		l.seq++
		runs = append(runs, &queuedRun{
			id: l.seq, firing: p.firing, fleet: fleetName, trigger: trigger.Name, agent: ag.Name,
			prompt: trigger.Prompt, cluster: target.KubeCluster, queuedAt: now, pending: true, def: ag,
		})
	}
	extra := map[string]int{}
	var start, waiting []*queuedRun
	for _, r := range runs {
		why, _ := l.waitLocked(c, st, r, extra[r.cluster], nil, now)
		if why != "" {
			r.why = why
			waiting = append(waiting, r)
			continue
		}
		extra[r.cluster]++
		start = append(start, r)
	}
	key := firedKey(fleetName, trigger.Name)
	if len(waiting) > 0 && l.firingsLocked(key) >= automationQueueMax {
		p.dropped = fmt.Sprintf("the trigger's queue is full (%d firings wait in it)", automationQueueMax)
		p.retryAfter = l.retryAfterLocked(c, waiting, now)
		// Nothing of a dropped firing runs, its other agents' runs neither:
		// the sender's retry (or the next firing) brings them all.
		p.direct, p.refused = nil, nil
		return p
	}
	p.marks = map[string]*startMark{}
	for _, r := range start {
		l.reserveLocked(p, r, now)
	}
	p.start = start
	p.queued = waiting
	l.queues[key] = append(l.queues[key], waiting...)
	return p
}

// refusal says why ag, a cluster agent, cannot run at all now: its
// connection is gone, or the login check refuses it.
func refusal(c runChecks, st *state.State, fleetName string, ag fleet.Agent) string {
	target := ag.DeployTarget()
	lim, ok := c.limits(target.KubeCluster)
	if !ok {
		return fmt.Sprintf("cluster %s is no longer connected", target.KubeCluster)
	}
	if c.login != nil {
		return c.login(st, fleetName, ag, lim.name)
	}
	return ""
}

// waitLocked says why r cannot start now ("" when it can), with extra more
// runs on its connection about to be created, and for a wait on the fire
// interval alone, when it ends. blocked are the agents of r's queue with an
// earlier run still waiting (nil: look at the queue).
func (l *automationLimiter) waitLocked(c runChecks, st *state.State, r *queuedRun, extra int, blocked map[string]bool, now time.Time) (string, time.Time) {
	lim, _ := c.limits(r.cluster)
	where := kubeGlyph + " " + lim.name
	if blocked != nil {
		if blocked[r.agent] {
			return "an earlier firing of this trigger waits for agent " + r.agent, time.Time{}
		}
	} else {
		for _, q := range l.queues[firedKey(r.fleet, r.trigger)] {
			if q.agent == r.agent && q.id < r.id {
				return "an earlier firing of this trigger waits for agent " + r.agent, time.Time{}
			}
		}
	}
	if lim.maxPerAgent > 0 {
		if n := len(l.holds[agentKey(r.fleet, r.agent)]); n >= lim.maxPerAgent {
			return fmt.Sprintf("agent %s has %d runs on %s, its limit (automation_max_per_agent)", r.agent, n, where), time.Time{}
		}
	}
	if c.quota != nil {
		if err := c.quota(st, r.cluster, l.creating[r.cluster]+extra); err != nil {
			return status.Convert(err).Message(), time.Time{}
		}
	}
	if lim.interval > 0 {
		if m, ok := l.starts[startKey(r.fleet, r.trigger, r.cluster)]; ok && m.firing != r.firing {
			if until := m.at.Add(lim.interval); now.Before(until) {
				return fmt.Sprintf("the trigger started a run on %s at %s, and starts one there every %s (automation_fire_interval)",
					where, m.at.UTC().Format(time.TimeOnly), lim.interval), until
			}
		}
	}
	return "", time.Time{}
}

// reserveLocked has r, about to start, hold its agent's slot, count as a
// create on its connection and mark the trigger's start there.
func (l *automationLimiter) reserveLocked(p *firingPlan, r *queuedRun, now time.Time) {
	sk := startKey(r.fleet, r.trigger, r.cluster)
	if p != nil && p.marks != nil {
		if _, saved := p.marks[sk]; !saved {
			if prev, ok := l.starts[sk]; ok {
				p.marks[sk] = &prev
			} else {
				p.marks[sk] = nil
			}
		}
	}
	l.starts[sk] = startMark{at: now, firing: r.firing}
	l.holdLocked(agentKey(r.fleet, r.agent), r.reservation())
	l.creating[r.cluster]++
}

// firingsLocked counts the firings waiting in the queue key.
func (l *automationLimiter) firingsLocked(key string) int {
	seen := map[uint64]bool{}
	for _, q := range l.queues[key] {
		seen[q.firing] = true
	}
	return len(seen)
}

// retryAfterLocked is when a dropped firing's runs might find room: the end
// of the fire interval of the first one's connection (30s without one).
func (l *automationLimiter) retryAfterLocked(c runChecks, waiting []*queuedRun, now time.Time) time.Duration {
	for _, r := range waiting {
		lim, _ := c.limits(r.cluster)
		if lim.interval <= 0 {
			continue
		}
		if m, ok := l.starts[startKey(r.fleet, r.trigger, r.cluster)]; ok {
			if left := m.at.Add(lim.interval).Sub(now); left > time.Second {
				return left
			}
		}
		return lim.interval
	}
	return fleet.DefaultKubeAutomationFireInterval
}

// commit makes a plan's queued runs the queue's to drain, recording the
// firing they belong to.
func (l *automationLimiter) commit(p *firingPlan, ev *triggerEvent, eventLog string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range append(append([]*queuedRun(nil), p.start...), p.queued...) {
		r.event, r.eventLog, r.pending = ev, eventLog, false
	}
}

// discard undoes the plans of a delivery that never reached the scheduler:
// their queued runs leave the queue, the reserved ones give their slots and
// start marks back.
func (l *automationLimiter) discard(plans []*firingPlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range plans {
		if p == nil {
			continue
		}
		for _, r := range p.start {
			l.releaseLocked(r.reservation())
			l.creatingDoneLocked(r.cluster)
		}
		for sk, prev := range p.marks {
			if m, ok := l.starts[sk]; !ok || m.firing != p.firing {
				continue
			}
			if prev == nil {
				delete(l.starts, sk)
			} else {
				l.starts[sk] = *prev
			}
		}
		key := firedKey(p.fleet, p.trigger.Name)
		kept := l.queues[key][:0]
		for _, q := range l.queues[key] {
			if q.firing != p.firing {
				kept = append(kept, q)
			}
		}
		l.setQueueLocked(key, kept)
	}
}

func (l *automationLimiter) setQueueLocked(key string, runs []*queuedRun) {
	if len(runs) == 0 {
		delete(l.queues, key)
		return
	}
	l.queues[key] = runs
}

func (l *automationLimiter) creatingDoneLocked(cluster string) {
	if l.creating[cluster] > 1 {
		l.creating[cluster]--
		return
	}
	delete(l.creating, cluster)
}

// started settles a run reserved to start once its create returned: its
// reservation becomes the watched run's slot (holder "fleet/instance"), or
// is given back when no instance was created.
func (l *automationLimiter) started(r *queuedRun, holder string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseLocked(r.reservation())
	l.creatingDoneLocked(r.cluster)
	if holder != "" {
		l.holdLocked(agentKey(r.fleet, r.agent), holder)
	}
}

// requeue puts a run whose create the connection's quotas refused after all
// back in its trigger's queue, in firing order.
func (l *automationLimiter) requeue(r *queuedRun, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseLocked(r.reservation())
	l.creatingDoneLocked(r.cluster)
	r.why, r.pending = why, false
	l.insertLocked(r)
}

// insertLocked adds r to its trigger's queue in id order.
func (l *automationLimiter) insertLocked(r *queuedRun) {
	key := firedKey(r.fleet, r.trigger)
	q := append(l.queues[key], r)
	sort.SliceStable(q, func(i, j int) bool { return q[i].id < q[j].id })
	l.queues[key] = q
	if r.id > l.seq {
		l.seq = r.id
	}
	if r.firing > l.seq {
		l.seq = r.firing
	}
}

// queueDrain is what one look at the queues found.
type queueDrain struct {
	// start are the runs reserved to start now; refused the ones that can
	// never run (and why, in their why); expired the ones that waited
	// automationQueueTTL.
	start, refused, expired []*queuedRun
	// next is when the earliest run waiting on a fire interval alone may
	// start (zero when none does).
	next time.Time
}

// due takes the runs out of the queues that may start now (reserving
// them), that are refused now (their agent, its connection or its login is
// gone, or their trigger no longer fires it), or that expired, in firing
// order, leaving the others waiting with why they wait. The agents and
// triggers are as the state load reads (under the lock, as plan's) has
// them.
func (l *automationLimiter) due(c runChecks, load func() *state.State, now time.Time) queueDrain {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := loadOrEmpty(load)
	agentOf := func(fleetName, agentName string) (fleet.Agent, bool) {
		if f := st.Fleets[fleetName]; f != nil {
			return fleet.FindAgent(f.Settings.Agents, agentName)
		}
		return fleet.Agent{}, false
	}
	// noLongerFires says why the run's trigger, as the state has it now,
	// would not fire its agent ("" when it would): a disabled or deleted
	// trigger, or one that no longer names the agent, starts none of the
	// runs it queued.
	noLongerFires := func(r *queuedRun) string {
		t, ok := fleet.FindTrigger(st.Fleets[r.fleet].Settings.Triggers, r.trigger)
		switch {
		case !ok:
			return fmt.Sprintf("trigger %s no longer exists", r.trigger)
		case t.Disabled:
			return fmt.Sprintf("trigger %s is disabled", r.trigger)
		case !slices.Contains(t.AgentNames, r.agent):
			return fmt.Sprintf("trigger %s no longer fires agent %s", r.trigger, r.agent)
		}
		return ""
	}
	var d queueDrain
	keys := make([]string, 0, len(l.queues))
	for k := range l.queues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		blocked := map[string]bool{}
		var kept []*queuedRun
		for _, r := range l.queues[key] {
			if r.pending {
				blocked[r.agent] = true
				kept = append(kept, r)
				continue
			}
			if now.Sub(r.queuedAt) >= automationQueueTTL {
				d.expired = append(d.expired, r)
				continue
			}
			ag, ok := agentOf(r.fleet, r.agent)
			if !ok {
				r.why = fmt.Sprintf("agent %s no longer exists", r.agent)
				d.refused = append(d.refused, r)
				continue
			}
			if why := noLongerFires(r); why != "" {
				r.why = why
				d.refused = append(d.refused, r)
				continue
			}
			target := ag.DeployTarget()
			if target.Backend != fleet.BackendKubernetes {
				// Moved off its cluster while it waited: it starts as any
				// other agent's run does.
				r.def, r.cluster = ag, ""
				d.start = append(d.start, r)
				l.holdLocked(agentKey(r.fleet, r.agent), r.reservation())
				continue
			}
			r.cluster = target.KubeCluster
			if why := refusal(c, st, r.fleet, ag); why != "" {
				r.why = why
				d.refused = append(d.refused, r)
				continue
			}
			// A run this drain reserved already counts as a create on its
			// connection (reserveLocked), so no extra is added here.
			why, until := l.waitLocked(c, st, r, 0, blocked, now)
			if why != "" {
				r.why = why
				blocked[r.agent] = true
				if !until.IsZero() && (d.next.IsZero() || until.Before(d.next)) {
					d.next = until
				}
				kept = append(kept, r)
				continue
			}
			r.def = ag
			l.reserveLocked(nil, r, now)
			d.start = append(d.start, r)
		}
		l.setQueueLocked(key, kept)
	}
	return d
}

// queued is every run waiting in the queues (pending ones aside), in id
// order, for the watch list.
func (l *automationLimiter) queuedRuns() []*queuedRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*queuedRun
	for _, q := range l.queues {
		for _, r := range q {
			if !r.pending {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// restoreQueued puts runs a previous daemon queued back in their queues.
func (l *automationLimiter) restoreQueued(runs []*queuedRun) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range runs {
		l.insertLocked(r)
	}
}

// counts are how many runs each agent has queued and holding a slot, by
// agentKey.
func (l *automationLimiter) counts() (queued, running map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	queued, running = map[string]int{}, map[string]int{}
	for _, q := range l.queues {
		for _, r := range q {
			queued[agentKey(r.fleet, r.agent)]++
		}
	}
	for agent, holders := range l.holds {
		running[agent] = len(holders)
	}
	return queued, running
}

// splitAgentKey is agentKey's inverse.
func splitAgentKey(key string) (fleetName, agentName string) {
	fleetName, agentName, _ = strings.Cut(key, "\x00")
	return fleetName, agentName
}
