package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// schedule.go is the server-side automation scheduler (issue #188). It runs as
// an unconditional background loop (cron triggers must fire even when no TUI is
// connected — that is the whole point of automation) and:
//
//  1. fires due Schedule triggers, spawning a normal fleet instance per
//     referenced agent (they appear in the TUI like any other instance);
//  2. once that instance is running, launches the agent's command in a fresh
//     tmux session (so the user can open it in the TUI), with the trigger prompt
//     + agent system prompt substituted into ${PROMPT}/${SYS_PROMPT} — the prompt
//     rides the command itself (e.g. `claude ... '${PROMPT}'`), so no keystroke
//     injection is needed;
//  3. reaps agents that go idle for longer than the idle timeout, using the same
//     agent-state detector factory the rest of the app uses (Claude/Auggie hooks,
//     screen-diff fallback).
//
// Webhook triggers are modeled but not delivered here — wiring the gateway
// endpoint is explicitly out of scope for issue #188.
//
// The loop is single-goroutine: scheduler's maps are only ever touched from the
// tick, so they need no locking. The live operations (instance create, command
// launch, activity capture, reap) are package-var seams so tests drive the
// lifecycle deterministically without containers.
var (
	// scheduleTickInterval is how often the loop samples cron triggers and
	// services watched agents. ~30s comfortably samples every minute-granular
	// cron minute at least once while staying cheap. Overridable via
	// FLEET_AUTOMATION_TICK_INTERVAL (a Go duration) so the integration test can
	// drive the spawn→launch→reap lifecycle in seconds instead of minutes.
	scheduleTickInterval = envDurationDefault("FLEET_AUTOMATION_TICK_INTERVAL", 30*time.Second)
	// automationIdleTimeout is how long an agent may stay inactive before its
	// instance is torn down (issue #188: "inactive for more than 2 minutes").
	// Overridable via FLEET_AUTOMATION_IDLE_TIMEOUT (a Go duration) for the same
	// reason — production keeps the 2m default.
	automationIdleTimeout = envDurationDefault("FLEET_AUTOMATION_IDLE_TIMEOUT", 2*time.Minute)
	// automationCreateTimeout bounds an automation run's create on a
	// cluster: a run still creating past it is destroyed (its pod and disk
	// with it) and the firing's trigger log says so, so a create stuck on a
	// paid cluster (an unschedulable pod, a slow pull) stops costing.
	// Overridable via FLEET_AUTOMATION_CREATE_TIMEOUT (a Go duration). Runs
	// on the other backends keep no deadline, as before.
	automationCreateTimeout = envDurationDefault("FLEET_AUTOMATION_CREATE_TIMEOUT", 20*time.Minute)
)

// envDurationDefault returns the Go duration parsed from the named env var, or
// def when it is unset, blank, or unparseable (a non-positive value is also
// rejected). These knobs exist only so the integration test can shorten the
// scheduler's cadence and idle timeout to verify the full lifecycle quickly;
// the daemon inherits the spawning client's environment (spawn.go), so setting
// them before `fleet ls` reaches the scheduler.
func envDurationDefault(name string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// maxAutomationProbeConcurrency bounds how many agent activity probes run at
// once per tick, so a large watch set fans out without spawning an unbounded
// number of concurrent container execs.
const maxAutomationProbeConcurrency = 8

// watchedAgent tracks one automation-spawned instance from creation through
// launch to idle reaping. The watch list is persisted across daemon restarts
// (automation_watch.go).
type watchedAgent struct {
	fleet        string
	instance     string
	agent        string // the agent's name, for the persisted record
	command      string
	prompt       string
	systemPrompt string
	spawnedAt    time.Time
	lastActive   time.Time
	launched     bool
	// detector + detectorTool are the per-agent activity detector, chosen by the
	// shared agentdetect factory from the probed tool (Claude/Auggie hooks, or the
	// screen-diff fallback). Lazily created/replaced in automationActivity.
	detector     agentdetect.Detector
	detectorTool state.AgentTool
	// event describes the trigger firing that spawned this agent. At launch its
	// payload is written into the instance and a note pointing at that file is
	// appended to the prompt, so the agent knows what fired it. Nil for an
	// adopted run (one the scheduler found rather than spawned).
	event *triggerEvent
	// eventLog names the firing's trigger event log (logTriggerEvent), from
	// which a restarted daemon reads the payload of a run it has yet to launch.
	eventLog string
	// adopted marks a run the scheduler took over at start without having a
	// record of it (an Automated instance missing from watch.json). It is never
	// launched; hasAgentSession, refreshed by each activity probe, tells whether
	// its agent session still exists (see idleLimit).
	adopted         bool
	hasAgentSession bool
}

// triggerEvent carries the context of the trigger firing that spawned an agent:
// what fired it (schedule / webhook / bash), when, and — for the body-bearing
// types — the payload (a webhook's request body, or a bash probe's stdout).
// launchAutomationCommand materializes payload() into a file inside the instance
// and tells the agent where to read it (appendEventPrompt), so the agent can act
// on the actual event rather than the static prompt alone.
type triggerEvent struct {
	kind        fleet.TriggerType
	triggerName string
	firedAt     time.Time
	webhookName string // webhook only
	body        []byte // webhook request body or bash stdout; nil for schedule triggers
}

// payload returns the bytes written to the in-instance event file: the body for
// the body-bearing types (a webhook's request body, a bash probe's stdout), or
// the fire time for a schedule trigger (which carries no external payload). A bash
// probe that fires with no stdout (e.g. `test -s file`) also falls back to the
// fire time, so the agent gets a meaningful event file rather than the empty one
// appendEventPrompt would otherwise point it at. The webhook path is left as-is —
// an empty webhook body stays empty (changing it is out of scope here).
func (e *triggerEvent) payload() []byte {
	if e.kind == fleet.TriggerSchedule || (e.kind == fleet.TriggerBash && len(e.body) == 0) {
		return []byte(e.firedAt.UTC().Format(time.RFC3339) + "\n")
	}
	return e.body
}

func (w *watchedAgent) key() string { return w.fleet + "/" + w.instance }

// scheduler holds the loop's per-process state (single-goroutine, no locks;
// lim, which the webhook receiver shares, has its own).
type scheduler struct {
	lastFired map[string]time.Time     // "fleet\x00trigger" -> minute it last fired
	watched   map[string]*watchedAgent // "fleet/instance" -> tracked agent
	// watchPath is where the watch list and lastFired persist across restarts
	// (automation_watch.go); "" keeps them in memory only, as tests do. saved is
	// the last content written, so an unchanged list is not rewritten.
	watchPath string
	saved     []byte
	// lim is the automation limiter (automation_limits.go): the service's
	// in the serve loop, a scheduler's own in tests.
	lim *automationLimiter
}

func newScheduler() *scheduler {
	return &scheduler{
		lastFired: make(map[string]time.Time),
		watched:   make(map[string]*watchedAgent),
		lim:       newAutomationLimiter(),
	}
}

// watch tracks w, holding a slot of its agent's.
func (sched *scheduler) watch(w *watchedAgent) {
	sched.watched[w.key()] = w
	sched.lim.hold(w.fleet, w.agent, w.key())
}

// unwatch stops tracking the run under k and gives its agent's slot back.
func (sched *scheduler) unwatch(k string) {
	delete(sched.watched, k)
	sched.lim.release(k)
}

// triggerFireBuffer is how many matched fires (webhook events or passing bash
// probes) may queue between scheduler drains before the webhook receiver starts
// shedding (503). The scheduler drains one batch per loop turn, so this only
// fills under a sustained burst.
const triggerFireBuffer = 64

// triggerFire is a matched trigger handed from a concurrent producer (the
// webhook receiver, or a bash probe goroutine) to the single-goroutine scheduler,
// which spawns the trigger's agents. The producer has already decided the trigger
// should fire (a webhook filter matched, or a bash probe exited 0); body carries
// the resulting payload (the webhook request body or the probe's stdout) through
// so each spawned agent can be handed the event it fired on (written into its
// instance and referenced from the prompt).
type triggerFire struct {
	fleet   string
	trigger fleet.Trigger
	body    []byte
	// plan is the firing as the webhook receiver planned it (and reserved
	// its runs) before it replied; nil for a bash probe's fire, which the
	// scheduler plans when it takes it.
	plan *firingPlan
}

// handFires hands a batch of fires to the scheduler, waiting while ctx
// lasts and, with a timeout above zero, at most that long. It reports
// whether the scheduler has the batch: false when the wait ran out, or once
// the scheduler stopped taking fires (closeFires).
func (s *service) handFires(ctx context.Context, fires []triggerFire, timeout time.Duration) bool {
	s.firesMu.RLock()
	defer s.firesMu.RUnlock()
	if s.firesClosed {
		return false
	}
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case s.triggerFires <- fires:
		return true
	case <-ctx.Done():
		return false
	case <-expired:
		return false
	}
}

// closeFires stops the hand-over of fires and returns the batches handed
// over that the scheduler has not taken. It takes them while the senders
// in the middle of handFires finish, so none of them waits on a full
// buffer: each batch is either returned here or refused to its sender.
func (s *service) closeFires() [][]triggerFire {
	closed := make(chan struct{})
	go func() {
		s.firesMu.Lock()
		s.firesClosed = true
		s.firesMu.Unlock()
		close(closed)
	}()
	var left [][]triggerFire
	for {
		select {
		case batch := <-s.triggerFires:
			left = append(left, batch)
		case <-closed:
			for {
				select {
				case batch := <-s.triggerFires:
					left = append(left, batch)
				default:
					return left
				}
			}
		}
	}
}

// bashProbeTimeout bounds how long a bash trigger's command may run before it is
// killed (and treated as not-fired). A polling command that hangs must not leak a
// process/goroutine or stall the next poll. Overridable via
// FLEET_AUTOMATION_BASH_TIMEOUT (a Go duration). The 50s default usually keeps a
// per-minute trigger's runs from overlapping (a probe started near the top of the
// minute finishes within it), but overlap is NOT guaranteed — e.g. a probe
// launched on the :30 tick right after daemon start, or a raised timeout — so a
// bash script should be idempotent. bashProbeSem bounds total probe concurrency
// regardless.
var bashProbeTimeout = envDurationDefault("FLEET_AUTOMATION_BASH_TIMEOUT", 50*time.Second)

// maxBashOutputSize caps how many bytes of a probe's stdout are retained. The
// stdout becomes the event payload (copied into the agent's instance and
// persisted to the trigger log), so an unbounded capture would let a runaway
// script (`yes`, a huge `curl`) OOM the daemon. Mirrors the webhook path's
// maxWebhookBodySize bound; output past it is discarded (see cappedBuffer).
const maxBashOutputSize = 1 << 20

// maxBashStderrSize caps a probe's captured stderr. Unlike stdout, stderr is not
// the payload — it is only folded into the error logged when a probe fails — so it
// gets a much smaller cap, enough to diagnose a failure without letting verbose
// stderr bloat a single log line.
const maxBashStderrSize = 4 << 10

// maxBashProbeConcurrency bounds how many bash trigger commands run at once, so a
// burst of due bash triggers can't spawn an unbounded number of host processes.
const maxBashProbeConcurrency = 8

// bashProbeSem bounds concurrent bash probes to maxBashProbeConcurrency. A
// process-wide semaphore (there is one scheduler per process); a probe acquires
// before running its command and releases when done.
var bashProbeSem = make(chan struct{}, maxBashProbeConcurrency)

// runScheduler is the automation loop; it returns when ctx is cancelled. It
// ticks once immediately so a trigger whose cron matches the current minute
// fires promptly on daemon start instead of waiting up to a full interval, and
// drains async fires (webhook events, passing bash probes) between ticks so a
// produced fire spawns its agents promptly. Both paths run on THIS goroutine, so
// sched's maps stay lock-free.
//
// It starts from the watch list a previous daemon persisted (scheduler.restore),
// so runs spawned before a restart are still launched and reaped.
//
// Besides its ticks, it looks again when a queued run's fire interval ends
// (so a queue drains at the pace of its interval, not of the tick) and when
// a settings change wakes it (wakeScheduler).
//
// As it stops it takes no more fires, and keeps the ones handed over and
// not taken yet for the next daemon (holdFires).
func (s *service) runScheduler(ctx context.Context) {
	sched := newScheduler()
	sched.lim = s.limiter()
	sched.watchPath = state.AutomationWatchPath()
	sched.restore(time.Now())
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	due := time.NewTimer(time.Hour)
	due.Stop()
	defer due.Stop()
	for {
		next := s.schedulerTick(ctx, sched, time.Now())
		sched.persist()
		due.Stop()
		if !next.IsZero() {
			due.Reset(max(time.Until(next), 0))
		}
		var batch []triggerFire
		select {
		case <-ctx.Done():
		case <-ticker.C:
		case <-due.C:
		case <-s.schedulerWake:
		case batch = <-s.triggerFires:
		}
		// A select picks any ready case, so a stopping scheduler can come
		// out of it with a batch: it waits for the next daemon too.
		if ctx.Err() != nil {
			left := s.closeFires()
			if batch != nil {
				left = append([][]triggerFire{batch}, left...)
			}
			s.holdFires(sched, left, time.Now())
			return
		}
		if batch != nil {
			s.fireTriggerBatch(sched, batch, time.Now())
			sched.persist()
		}
	}
}

// fireTriggerBatch spawns the agents of every fire in a batch, on the SCHEDULER
// goroutine (so it touches sched.watched without locking, like the schedule
// path). The producer already decided each trigger should fire (a matched webhook
// filter, or a passing bash probe); here we just resolve each fleet's CURRENT
// agents (state may have changed since the fire was produced) and spawn them. The
// next loop iteration's schedulerTick reloads state and services the
// freshly-watched agents (launch on running, reap on idle), so async-fired agents
// ride the exact same lifecycle as scheduled ones.
//
// A webhook's fires come planned (the receiver reserved their runs before it
// replied); a bash probe's are planned here.
func (s *service) fireTriggerBatch(sched *scheduler, batch []triggerFire, now time.Time) {
	if len(batch) == 0 {
		return
	}
	s.planFires(sched, batch, now)
	for _, f := range batch {
		s.runFiring(sched, f.plan, f.body, now)
	}
}

// holdFires keeps for the next daemon the batches the scheduler did not
// take before it stopped (closeFires): their senders were told the firings
// were taken (a webhook's 200 or 202), so a stop must not lose them. Each
// firing is recorded in its trigger's log, and the runs it would start now
// wait in its trigger's queue, which the watch list keeps. A daemon killed
// outright still loses what was in its buffer.
func (s *service) holdFires(sched *scheduler, batches [][]triggerFire, now time.Time) {
	if len(batches) == 0 {
		return
	}
	firings := 0
	for _, batch := range batches {
		s.planFires(sched, batch, now)
		for _, f := range batch {
			s.holdFiring(sched, f.plan, f.body, now)
		}
		firings += len(batch)
	}
	sched.persist()
	flog.Info("automation: firings kept for the next start", "firings", firings)
}

// planFires plans the fires of batch that come unplanned (a bash probe's).
func (s *service) planFires(sched *scheduler, batch []triggerFire, now time.Time) {
	var unplanned []firingItem
	var at []int
	for i, f := range batch {
		if f.plan == nil {
			unplanned = append(unplanned, firingItem{fleet: f.fleet, trigger: f.trigger})
			at = append(at, i)
		}
	}
	if len(unplanned) > 0 {
		for i, p := range sched.lim.plan(s.runChecks(), loadScheduleState, unplanned, now) {
			batch[at[i]].plan = p
		}
	}
}

// loadScheduleState is scheduleLoadState for the limiter: nil when the
// state does not read (the limiter then plans against no records).
func loadScheduleState() *state.State {
	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("automation: load state failed", "err", err)
		return nil
	}
	return st
}

// runChecks are the daemon's answers to the limiter's questions about a
// cluster run (automation_limits.go).
func (s *service) runChecks() runChecks {
	return runChecks{
		limits: func(id string) (clusterLimits, bool) {
			if s.kube == nil {
				return clusterLimits{}, false
			}
			return s.kube.automationLimits(id)
		},
		quota: func(st *state.State, id string, extra int) error {
			if s.kube == nil {
				return nil
			}
			return s.kube.quotaRoom(st, id, extra)
		},
		login: loginCheck,
	}
}

// scheduleLoadState loads the persisted state for the scheduler. It is a seam so
// tests can drive schedulerTick deterministically.
var scheduleLoadState = state.Load

// schedulerTick fires due triggers, services watched agents and starts the
// queued runs the limits allow for one tick, returning when the next queued
// run's fire interval ends (zero when none waits on one).
func (s *service) schedulerTick(ctx context.Context, sched *scheduler, now time.Time) time.Time {
	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("automation: load state failed", "err", err)
		return time.Time{}
	}
	fired := false
	dues := dueCronTriggers(st.Fleets, now, sched.lastFired)
	if len(dues) > 0 {
		// Record the fire minutes before acting on them, so a daemon restarted
		// within the minute neither fires them again nor re-runs a bash probe.
		sched.persist()
	}
	for _, due := range dues {
		agents := agentsForTrigger(st.Fleets[due.fleet], due.trigger)
		if due.trigger.Type == fleet.TriggerBash {
			// A bash trigger's cron only schedules a poll — it fires its agents only
			// if its command exits 0. Skip the probe entirely when no live agents
			// would receive the fire: the command has side effects, so don't run it
			// just to discard the result. (The set is re-resolved at fire time too.)
			// Otherwise run the command OFF the tick (it may be slow or block); on a
			// zero exit the probe hands the trigger back via triggerFires to spawn the
			// agents (see startBashProbe). Nothing is spawned here, so `fired` stays.
			if len(agents) > 0 {
				startBashProbe(s, due.fleet, due.trigger)
			}
			continue
		}
		// Schedule triggers carry no external payload — the event file gets the
		// fire time (see triggerEvent.payload), so pass a nil body here.
		if s.fireTriggerAgents(sched, due.fleet, due.trigger, agents, now, nil) {
			fired = true
		}
	}
	// createAutomationInstance writes the new StatusCreating record synchronously,
	// but the snapshot loaded above predates it — so reload before servicing the
	// watch set, otherwise findInstance can't see a just-created instance and the
	// watch entry is dropped the same tick it was added (and the agent never
	// launches).
	if fired {
		if reloaded, err := scheduleLoadState(); err == nil {
			st = reloaded
		}
	}
	s.serviceWatched(ctx, sched, st, now)
	next := s.serviceQueue(sched, now)
	s.publishAutomationStatus(sched)
	return next
}

// fireTriggerAgents spawns one instance per agent the trigger activates and
// registers each in the watch set, returning whether anything was spawned (the
// caller reloads state when so, see schedulerTick). body is the webhook request
// payload (nil for schedule triggers); it rides each watched agent's event so
// the launch can hand it to the agent. Shared by the schedule path and the
// webhook path; it only mutates sched.watched, so it MUST run on the scheduler
// goroutine.
func (s *service) fireTriggerAgents(sched *scheduler, fleetName string, trigger fleet.Trigger, agents []fleet.Agent, now time.Time, body []byte) bool {
	if len(agents) == 0 {
		return false
	}
	plans := sched.lim.plan(s.runChecks(), loadScheduleState, []firingItem{{fleet: fleetName, trigger: trigger, agents: agents}}, now)
	return s.runFiring(sched, plans[0], body, now)
}

// runFiring carries out a planned firing on the scheduler goroutine: it
// records the firing in its trigger's event log (logTriggerEvent), starts
// the runs the plan starts, and appends each agent's outcome to the log as
// it happens: the instance it got, or why it got none (refused, queued,
// dropped, not created). It returns whether it created an instance.
func (s *service) runFiring(sched *scheduler, p *firingPlan, body []byte, now time.Time) bool {
	ev, eventLog := s.recordFiring(sched, p, body, now)
	if ev == nil || p.dropped != "" {
		return false
	}
	fired := false
	for _, ag := range p.direct {
		if s.startRun(sched, p.fleet, ag, p.trigger.Prompt, ev, eventLog, nil, now) {
			fired = true
		}
	}
	for _, r := range p.start {
		if s.startRun(sched, p.fleet, r.def, r.prompt, ev, eventLog, r, now) {
			fired = true
		}
	}
	if fired {
		// Persist the new runs at once: their CREATING records already exist.
		sched.persist()
	}
	return fired
}

// holdFiring records a firing the scheduler takes as it stops (holdFires)
// as runFiring does, but the runs it would start now wait in its trigger's
// queue instead, for the next daemon to start.
func (s *service) holdFiring(sched *scheduler, p *firingPlan, body []byte, now time.Time) {
	ev, eventLog := s.recordFiring(sched, p, body, now)
	if ev == nil || p.dropped != "" {
		return
	}
	for _, r := range sched.lim.postpone(p, ev, eventLog, "the daemon stopped before it started the run; the next one starts it", now) {
		flog.Info("automation: run queued", "fleet", p.fleet, "trigger", p.trigger.Name, "agent", r.agent, "why", r.why)
		appendTriggerOutcome(p.fleet, p.trigger.Name, eventLog, now, r.agent, "queued: "+r.why)
	}
}

// recordFiring records a planned firing in its trigger's event log
// (logTriggerEvent) with the outcomes of the runs it does not start: all of
// them when it was dropped, else the refused and the queued ones. It
// returns the firing's event and log, or a nil event when the plan has
// nothing to record.
func (s *service) recordFiring(sched *scheduler, p *firingPlan, body []byte, now time.Time) (*triggerEvent, string) {
	if p == nil {
		return nil, ""
	}
	if p.dropped == "" && len(p.direct)+len(p.start)+len(p.queued)+len(p.refused) == 0 {
		flog.Warn("automation: fired trigger has no live agents", "fleet", p.fleet, "trigger", p.trigger.Name)
		return nil, ""
	}
	// One event describes this firing; it's shared (read-only) by every agent the
	// trigger spawns and recorded once to the trigger's on-host event log so the
	// firing is debuggable after the instances are reaped (see trigger_logs.go).
	ev := &triggerEvent{
		kind:        p.trigger.Type,
		triggerName: p.trigger.Name,
		firedAt:     now,
		webhookName: p.trigger.WebhookName,
		body:        body,
	}
	eventLog := logTriggerEvent(p.fleet, ev)
	sched.lim.commit(p, ev, eventLog)
	outcome := func(agent, msg string) {
		appendTriggerOutcome(p.fleet, p.trigger.Name, eventLog, now, agent, msg)
	}
	if p.dropped != "" {
		flog.Warn("automation: firing dropped", "fleet", p.fleet, "trigger", p.trigger.Name, "why", p.dropped)
		outcome(strings.Join(p.trigger.AgentNames, ","), "dropped: "+p.dropped)
		return ev, eventLog
	}
	for _, r := range p.refused {
		flog.Warn("automation: run refused", "fleet", p.fleet, "trigger", p.trigger.Name, "agent", r.agent, "why", r.why)
		outcome(r.agent, "refused: "+r.why)
	}
	for _, r := range p.queued {
		flog.Info("automation: run queued", "fleet", p.fleet, "trigger", p.trigger.Name, "agent", r.agent, "why", r.why)
		outcome(r.agent, "queued: "+r.why)
	}
	return ev, eventLog
}

// startRun creates the instance of one agent run and watches it, appending
// the outcome to the firing's event log. r is the run's reservation for a
// cluster agent (nil for the others): a create the connection's quotas
// refuse after all puts it back in the queue.
func (s *service) startRun(sched *scheduler, fleetName string, ag fleet.Agent, prompt string, ev *triggerEvent, eventLog string, r *queuedRun, now time.Time) bool {
	trigger := ""
	if ev != nil {
		trigger = ev.triggerName
	}
	outcome := func(msg string) { appendTriggerOutcome(fleetName, trigger, eventLog, now, ag.Name, msg) }
	instName, err := createAutomationInstance(s, fleetName, ag, now)
	if err != nil {
		msg := status.Convert(err).Message()
		if r != nil && status.Code(err) == codes.ResourceExhausted {
			sched.lim.requeue(r, msg)
			flog.Info("automation: run queued", "fleet", fleetName, "trigger", trigger, "agent", ag.Name, "why", msg)
			outcome("queued: " + msg)
			return false
		}
		if r != nil {
			sched.lim.started(r, "")
		}
		flog.Warn("automation: create instance failed", "fleet", fleetName, "agent", ag.Name, "err", err)
		outcome("not created: " + msg)
		return false
	}
	flog.Info("automation: trigger fired", "fleet", fleetName, "trigger", trigger, "agent", ag.Name, "instance", instName)
	w := &watchedAgent{
		fleet:        fleetName,
		instance:     instName,
		agent:        ag.Name,
		command:      ag.Command,
		prompt:       prompt,
		systemPrompt: ag.SystemPrompt,
		spawnedAt:    now,
		lastActive:   now,
		event:        ev,
		eventLog:     eventLog,
	}
	sched.watched[w.key()] = w
	if r != nil {
		sched.lim.started(r, w.key())
	} else {
		sched.lim.hold(fleetName, ag.Name, w.key())
	}
	outcome(fmt.Sprintf("instance %s (%s)", instName, s.targetLabel(ag.DeployTarget())))
	return true
}

// targetLabel names a run's target in an outcome line: the backend, or
// "☸ <connection>" for a cluster.
func (s *service) targetLabel(t fleet.DeployTarget) string {
	if t.Backend != fleet.BackendKubernetes {
		return string(t.Backend)
	}
	if lim, ok := s.runChecks().limits(t.KubeCluster); ok && lim.name != "" {
		return kubeGlyph + " " + lim.name
	}
	return kubeGlyph + " " + t.KubeCluster
}

// serviceQueue starts the queued runs the limits now allow, in firing
// order, and records the ones that expired or can no longer run, returning
// when the next run waiting on a fire interval may start.
func (s *service) serviceQueue(sched *scheduler, now time.Time) time.Time {
	d := sched.lim.due(s.runChecks(), loadScheduleState, now)
	for _, r := range d.expired {
		flog.Warn("automation: queued run expired", "fleet", r.fleet, "trigger", r.trigger, "agent", r.agent, "why", r.why)
		appendTriggerOutcome(r.fleet, r.trigger, r.eventLog, now, r.agent,
			fmt.Sprintf("expired after %s in the queue; it waited because %s", automationQueueTTL, r.why))
	}
	for _, r := range d.refused {
		flog.Warn("automation: queued run refused", "fleet", r.fleet, "trigger", r.trigger, "agent", r.agent, "why", r.why)
		appendTriggerOutcome(r.fleet, r.trigger, r.eventLog, now, r.agent, "refused: "+r.why)
	}
	fired := false
	for _, r := range d.start {
		if s.startRun(sched, r.fleet, r.def, r.prompt, r.event, r.eventLog, r, now) {
			fired = true
		}
	}
	if fired || len(d.expired)+len(d.refused) > 0 {
		sched.persist()
	}
	return d.next
}

// scheduledFire is one cron trigger that is due now (a schedule or bash trigger).
type scheduledFire struct {
	fleet   string
	trigger fleet.Trigger
}

// dueCronTriggers returns the cron-driven triggers (schedule AND bash — see
// TriggerType.IsCron) whose cron matches now and that have not already fired this
// minute, recording the due in lastFired. It is the shared cron sampler: a
// schedule trigger that comes back fires its agents immediately, while a bash
// trigger first runs its command and fires only on a zero exit (the caller
// branches on type). Recording lastFired at due-detection time — before a bash
// command even runs — is what keeps the two ticks within one minute from polling
// the same command twice. Pure except for the lastFired bookkeeping, so it is
// unit-tested directly.
func dueCronTriggers(fleets map[string]*fleet.Fleet, now time.Time, lastFired map[string]time.Time) []scheduledFire {
	minute := now.Truncate(time.Minute)
	current := make(map[string]struct{})
	var out []scheduledFire
	for name, f := range fleets {
		if f == nil {
			continue
		}
		for _, t := range f.Settings.Triggers {
			if !t.Type.IsCron() {
				continue
			}
			key := firedKey(name, t.Name)
			current[key] = struct{}{}
			if t.Disabled {
				continue // disabled triggers are kept but never fire
			}
			if t.Cron == "" {
				continue
			}
			sched, err := fleet.ParseCron(t.Cron)
			if err != nil || !sched.Matches(now) {
				continue
			}
			if last, ok := lastFired[key]; ok && !last.Before(minute) {
				continue // already fired this minute
			}
			lastFired[key] = minute
			out = append(out, scheduledFire{fleet: name, trigger: t})
		}
	}
	// Drop lastFired entries for triggers that no longer exist (deleted, renamed,
	// or changed type) so the map can't grow without bound on a long-running daemon.
	for k := range lastFired {
		if _, ok := current[k]; !ok {
			delete(lastFired, k)
		}
	}
	return out
}

// agentsForTrigger resolves a trigger's agent names to the fleet's Agent records
// (in reference order, skipping any that no longer exist).
func agentsForTrigger(f *fleet.Fleet, t fleet.Trigger) []fleet.Agent {
	if f == nil {
		return nil
	}
	var out []fleet.Agent
	for _, name := range t.AgentNames {
		for _, a := range f.Settings.Agents {
			if a.Name == name {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// serviceWatched advances every tracked agent: launch its command once running,
// reap it once idle (tmux mode only), and drop it once gone. The per-agent
// activity probe is a potentially slow exec, so the probes run concurrently
// (bounded) — a large watch set can't stall the scheduler tick. All map
// mutations and reap decisions stay on this single goroutine; only the probes
// fan out, and each touches just its own detector + instance (and the
// mutex-guarded backendFor), so they are race-free.
func (s *service) serviceWatched(ctx context.Context, sched *scheduler, st *state.State, now time.Time) {
	// Phase 1 (sequential): lifecycle. Drop gone/failed, launch on running, drop
	// fire-and-forget agents. Collect the launched tmux agents needing a probe.
	type pendingCheck struct {
		key  string
		w    *watchedAgent
		inst *fleet.Instance
	}
	var checks []pendingCheck
	for k, w := range sched.watched {
		inst := findInstance(st, w.fleet, w.instance)
		if inst == nil {
			sched.unwatch(k) // instance gone (reaped / destroyed)
			continue
		}
		switch inst.Status {
		case fleet.StatusFailed:
			// Provisioning failed; stop tracking and leave the record for the
			// user (on a cluster, without its pod and disk).
			s.failedRun(w, inst, now)
			sched.unwatch(k)
			continue
		case fleet.StatusRunning:
			// fall through
		case fleet.StatusCreating:
			if createOverdue(w, inst, now) {
				s.abandonOverdueCreate(w, inst, now)
				sched.unwatch(k)
			}
			continue
		default:
			continue // transitional
		}

		if !w.launched {
			launchAutomationCommand(ctx, s, w, inst)
			w.launched = true
			w.lastActive = now
			continue
		}
		checks = append(checks, pendingCheck{key: k, w: w, inst: inst})
	}

	// Phase 2 (concurrent, bounded): probe each agent's activity.
	type probeResult struct {
		state agentdetect.State
		ok    bool
	}
	results := make([]probeResult, len(checks))
	sem := make(chan struct{}, maxAutomationProbeConcurrency)
	var wg sync.WaitGroup
	for i := range checks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			activity, ok := automationActivity(s, checks[i].w, checks[i].inst, now)
			results[i] = probeResult{state: activity, ok: ok}
		}(i)
	}
	wg.Wait()

	// Phase 3 (sequential): apply activity + reap idle agents.
	for i, c := range checks {
		r := results[i]
		if !r.ok {
			continue // transient capture failure — don't reset activity or reap
		}
		if r.state == agentdetect.StateWorking {
			c.w.lastActive = now
			continue
		}
		if idle := now.Sub(c.w.lastActive); idle >= c.w.idleLimit() {
			flog.Info("automation: reaping idle agent", "fleet", c.w.fleet, "instance", c.w.instance, "idle", idle.Round(time.Second).String())
			err := reapAutomationInstance(s, c.w.fleet, c.w.instance)
			recordTriggerOutcome(c.w, now, fmt.Sprintf("instance %s reaped after %s idle; %s", c.w.instance, idle.Round(time.Second), destroyWords(err)))
			sched.unwatch(c.key)
		}
	}
}

// createOverdue reports whether w's run, still creating, has outlived
// automationCreateTimeout: only a cluster run has the deadline.
func createOverdue(w *watchedAgent, inst *fleet.Instance, now time.Time) bool {
	return !w.launched && inst.Backend == fleet.BackendKubernetes && automationCreateTimeout > 0 &&
		now.Sub(w.spawnedAt) >= automationCreateTimeout
}

// abandonOverdueCreate destroys a run whose create outlived the deadline (the
// destroy cancels the create job and tears down what it made) and records
// the outcome in the firing's trigger log.
func (s *service) abandonOverdueCreate(w *watchedAgent, inst *fleet.Instance, now time.Time) {
	took := now.Sub(w.spawnedAt).Round(time.Second)
	flog.Warn("automation: create deadline passed; destroying the run", "fleet", w.fleet, "instance", w.instance, "agent", w.agent, "after", took.String(), "deadline", automationCreateTimeout.String())
	err := reapAutomationInstance(s, w.fleet, w.instance)
	recordTriggerOutcome(w, now, fmt.Sprintf("create of %s did not finish within %s (FLEET_AUTOMATION_CREATE_TIMEOUT); %s", inst.Name, automationCreateTimeout, destroyWords(err)))
}

// destroyWords says what became of a run's destroy (reapAutomationInstance's
// error), for its outcome line.
func destroyWords(err error) string {
	if err != nil {
		return "destroying it failed: " + status.Convert(err).Message()
	}
	return "it is being destroyed"
}

// interruptedCreate reports whether a FAILED record's error is a create a
// daemon restart interrupted (interrupted_jobs.go, kube_reconcile.go).
func interruptedCreate(inst *fleet.Instance) bool {
	return inst.Error == interruptedByRestart || strings.HasPrefix(inst.Error, "kubernetes:interrupted")
}

// failedRun deals with a run whose create failed, and records it in the
// firing's trigger log. On a cluster nobody will press Retry on an
// automation run: one a daemon restart interrupted is destroyed, and any
// other keeps its record and reason, as on Docker, but its pod and disk are
// released at once (releaseFailedRun), so a failed run holds no billable
// disk and counts against neither quota.
func (s *service) failedRun(w *watchedAgent, inst *fleet.Instance, now time.Time) {
	reason := strings.TrimSpace(inst.Error)
	if reason == "" {
		reason = "no reason recorded"
	}
	switch {
	case inst.Backend == fleet.BackendKubernetes && interruptedCreate(inst):
		flog.Warn("automation: destroying a run a daemon restart interrupted", "fleet", w.fleet, "instance", inst.Name)
		err := reapAutomationInstance(s, w.fleet, w.instance)
		recordTriggerOutcome(w, now, fmt.Sprintf("create of %s was interrupted by a daemon restart; %s", inst.Name, destroyWords(err)))
	case inst.Backend == fleet.BackendKubernetes:
		releaseFailedRun(s, w.fleet, inst)
		recordTriggerOutcome(w, now, fmt.Sprintf("create of %s failed: %s; the record is kept, its pod and disk are released", inst.Name, reason))
	case interruptedCreate(inst):
		recordTriggerOutcome(w, now, fmt.Sprintf("create of %s was interrupted by a daemon restart; the record is kept", inst.Name))
	default:
		recordTriggerOutcome(w, now, fmt.Sprintf("create of %s failed: %s", inst.Name, reason))
	}
}

// releaseFailedRun gives a failed cluster run's pod and disk back, keeping
// its FAILED record and reason. The record lets go of its instance key
// first, under the state lock (from then on nothing reaches the objects
// through it, and it counts against neither quota); then the objects are
// deleted by that key in the background, the key's tombstone letting the
// reconciler finish what the teardown cannot (an unreachable cluster). A
// package var so tests can see it called.
var releaseFailedRun = func(s *service, fleetName string, inst *fleet.Instance) {
	var held *fleet.Instance
	_ = state.Update(func(st *state.State) error {
		cur := findInstance(st, fleetName, inst.Name)
		if cur == nil || cur.Status != fleet.StatusFailed || cur.KubeKey == "" || cur.KubeKey != inst.KubeKey {
			return errNothingToRelease
		}
		c := *cur
		held = &c
		cur.KubeKey, cur.ContainerID = "", ""
		return nil
	})
	if held == nil {
		return
	}
	flog.Info("automation: releasing a failed run's pod and disk", "fleet", fleetName, "instance", held.Name, "cluster", held.KubeCluster, "key", held.KubeKey)
	s.pushState()
	go func() {
		if s.kubeRecovery != nil {
			s.kubeRecovery.destroying(fleetName, held)
		}
		if err := jobDownInstance(held); err != nil {
			flog.Warn("automation: releasing a failed run's pod and disk", "fleet", fleetName, "instance", held.Name, "err", err)
		}
		s.kubeRecovery.destroyed(held)
	}()
}

// errNothingToRelease aborts releaseFailedRun's state update: the record is
// gone, retried, or released already.
var errNothingToRelease = errors.New("nothing to release")

func findInstance(st *state.State, fleetName, instanceName string) *fleet.Instance {
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil {
		return nil
	}
	return inst
}

// automationSeq makes automation instance names unique within a second.
var automationSeq int64

// automationInstanceName builds a unique, valid instance name for an agent run.
func automationInstanceName(agentName string, now time.Time) string {
	base := sanitizeAutomationName(agentName)
	if base == "" {
		base = "agent"
	}
	return fmt.Sprintf("%s-%s-%d", base, now.Format("150405"), atomic.AddInt64(&automationSeq, 1))
}

// freeAutomationInstanceName is automationInstanceName, past the names the
// fleet's records hold: a daemon started again counts from 1 again, and a
// run of the one before (an interrupted create being destroyed, a firing
// restored from its queue started in the same second) can hold the name,
// which would fail the create ("already exists") and lose the run.
func freeAutomationInstanceName(fleetName, agentName string, now time.Time) string {
	st, _ := state.Load()
	for {
		name := automationInstanceName(agentName, now)
		if st == nil || st.Fleets[fleetName] == nil {
			return name
		}
		if _, err := st.Fleets[fleetName].GetInstance(name); err != nil {
			return name
		}
	}
}

// sanitizeAutomationName lowercases and reduces a name to [a-z0-9-] so it embeds
// cleanly in container names / workspace paths.
func sanitizeAutomationName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	return out
}

// --- Live operation seams (overridden in tests) ----------------------------

// createAutomationInstance spawns a new instance for an agent run and returns
// its name. It reuses the same job path as the CreateInstance RPC, on the
// agent's stored target: its backend and, for a Kubernetes agent, its
// cluster connection (whose quotas the create checks).
var createAutomationInstance = func(s *service, fleetName string, ag fleet.Agent, now time.Time) (string, error) {
	instName := freeAutomationInstanceName(fleetName, ag.Name, now)
	if _, err := s.startCreateInstanceJob(automationCreateRequest(fleetName, instName, ag), true); err != nil {
		return "", err
	}
	return instName, nil
}

// automationCreateRequest is the create of an agent run's instance.
func automationCreateRequest(fleetName, instName string, ag fleet.Agent) *fleetgrpc.CreateInstanceRequest {
	target := ag.DeployTarget()
	return &fleetgrpc.CreateInstanceRequest{
		Fleet:       fleetName,
		Instance:    instName,
		Backend:     protoconv.BackendToProto(target.Backend),
		KubeCluster: target.KubeCluster,
	}
}

// launchAutomationCommand starts the agent's command in a fresh tmux session
// inside its (running) instance, so it's viewable in the TUI. Both ${SYS_PROMPT}
// and ${PROMPT} are substituted into the command (the default passes the prompt
// as the agent's positional argument), so the command itself brings up a live
// agent already working on the prompt — no keystroke injection, no readiness
// polling.
//
// `tmux new-session -d -s X <cmd>` runs <cmd> as the session's process (tmux
// hands it to sh -c). It execs straight against the container (RunScript) as the
// session user — NOT the devcontainer Node CLI path, which from the daemon
// silently produced no tmux server. Runs off the tick because TmuxEnsureInstalled
// may apt-install tmux on first use.
var launchAutomationCommand = func(ctx context.Context, s *service, w *watchedAgent, inst *fleet.Instance) {
	session := tui.ResolveSessionName(inst.Name, "agent")
	prompt := w.prompt
	var eventPath string
	if w.event != nil {
		eventPath = automationEventPath(w.event.firedAt)
		prompt = appendEventPrompt(prompt, w.event, eventPath)
	}
	command := fleet.SubstituteAgentCommand(w.command, prompt, w.systemPrompt)
	script := buildAgentLaunchScript(session, command)
	b := s.hub.backendFor(inst)
	go func() {
		// Write the trigger payload into the instance BEFORE launching the agent,
		// so the file the prompt points at already exists the moment the agent
		// starts. Best-effort: a write failure still launches the agent (it just
		// finds no event file) rather than dropping the whole run.
		if w.event != nil {
			payload := w.event.payload()
			if err := writeAutomationEventFile(inst, eventPath, payload); err != nil {
				flog.Warn("automation: write event file failed", "instance", inst.Name, "path", eventPath, "err", err)
			} else {
				flog.Info("automation: event file written", "fleet", w.fleet, "instance", inst.Name, "path", eventPath, "bytes", len(payload))
			}
		}
		if out, err := b.RunScript(inst.ContainerID, script); err != nil {
			flog.Error("automation: launch agent failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
		} else {
			trigger := ""
			if w.event != nil {
				trigger = w.event.triggerName
			}
			flog.Info("automation: agent launched", "fleet", w.fleet, "instance", inst.Name, "trigger", trigger)
		}
	}()
}

// automationEventPath is where a run's trigger payload is written inside its
// instance. Each automation run gets a fresh container, so a single fixed-prefix,
// time-stamped name never collides within one instance.
func automationEventPath(firedAt time.Time) string {
	return "/tmp/fleet-event-" + firedAt.UTC().Format("20060102T150405Z")
}

// appendEventPrompt appends a note to the agent prompt naming what fired the run
// and where its payload was written, so the agent reads the actual event rather
// than working from the static prompt alone. A blank prompt becomes just the note.
func appendEventPrompt(prompt string, e *triggerEvent, path string) string {
	var detail string
	switch e.kind {
	case fleet.TriggerWebhook:
		detail = fmt.Sprintf("Its payload has been written to %s in this instance — read that file for the event details.", path)
	case fleet.TriggerBash:
		detail = fmt.Sprintf("The output of its bash probe has been written to %s in this instance — read that file for the event details.", path)
	default:
		detail = fmt.Sprintf("The time it fired has been written to %s in this instance.", path)
	}
	note := fmt.Sprintf("This session was started automatically by the %q %s trigger. %s", e.triggerName, e.kind, detail)
	if strings.TrimSpace(prompt) == "" {
		return note
	}
	return prompt + "\n\n---\n" + note
}

// writeAutomationEventFile writes data to path inside the instance via the
// backend's CopyFile strategy seam (copyFileInto) — the same stdin-EOF-safe
// transfer the fleet-copy handler uses. An earlier base64-over-stdin write hung
// forever on the coder backend, whose `coder ssh` never delivers the stdin EOF a
// `base64 -d` waits for (issue #223); routing through CopyFile streams over a
// clean channel on devcontainer/codespaces and transfers out-of-band with scp on
// coder, for a payload of any size. A package var so tests can stub it.
var writeAutomationEventFile = func(inst *fleet.Instance, path string, data []byte) error {
	return copyFileInto(inst, bytes.NewReader(data), path, 0o644)
}

// buildAgentLaunchScript builds the in-container snippet that brings up the agent
// in a fresh tmux session. The command runs via an INTERACTIVE bash (`bash -ic`):
// tmux otherwise runs a new-session command through a bare `sh -c`, which doesn't
// source ~/.bashrc, so an agent like Claude (installed under ~/.local/bin and
// added to PATH only in .bashrc) isn't found and the session dies instantly. The
// interactive shell sources .bashrc and has the agent on PATH — the same
// environment the user's own session shell has.
func buildAgentLaunchScript(sessionName, command string) string {
	launch := "bash -ic " + dotfiles.ShQuote(command)
	return dotfiles.TmuxEnsureInstalled +
		fmt.Sprintf("tmux new-session -d -s %s %s", dotfiles.ShQuote(sessionName), dotfiles.ShQuote(launch))
}

// automationActivity reports a watched agent's current activity, choosing the
// detector via the shared agentdetect factory from the probed tool — so Claude /
// Auggie use their lifecycle hooks and other tools fall back to screen-diff,
// identical to the live TUI path and improvable in one place. The bool is false
// on a transient capture failure, telling the caller to leave the agent
// untouched.
var automationActivity = func(s *service, w *watchedAgent, inst *fleet.Instance, now time.Time) (agentdetect.State, bool) {
	if inst.ContainerID == "" {
		return agentdetect.StateNotRunning, false
	}
	b := s.hub.backendFor(inst)
	caps := b.CaptureAllSessions(inst.ContainerID)
	if !caps.OK {
		return agentdetect.StateNotRunning, false
	}
	tool := state.AgentTool("")
	if t, ok := b.AgentToolProbe(inst.ContainerID); ok {
		tool = state.AgentTool(t)
	}
	if w.detector == nil || w.detectorTool != tool {
		w.detector = agentdetect.NewDetector(tool)
		w.detectorTool = tool
	}
	_, w.hasAgentSession = caps.Sessions[tui.ResolveSessionName(inst.Name, "agent")]
	return w.detector.Detect(caps, now), true
}

// reapAutomationInstance tears down an automation instance (container +
// record) via the same destroy job the CLI/TUI use, returning why the
// destroy could not start.
var reapAutomationInstance = func(s *service, fleetName, instanceName string) error {
	if _, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{
		Fleet:    fleetName,
		Instance: &instanceName,
	}); err != nil {
		flog.Error("automation: reap failed", "fleet", fleetName, "instance", instanceName, "err", err)
		return err
	}
	return nil
}

// startBashProbe runs a bash trigger's command off the scheduler goroutine and,
// on a zero exit, hands the trigger to the scheduler (via triggerFires) to fire
// its agents — the command's stdout becomes the event payload. A non-zero exit, a
// timeout, or an exec error means the polled condition is not met, so nothing
// fires. It runs asynchronously (a slow polling command must not stall the
// scheduler tick, mirroring the webhook delivery path) and is bounded by
// bashProbeSem so a burst of due bash triggers can't spawn unbounded host
// processes. A package var so schedulerTick tests can stub it. (now is the cron
// due-time, taken when runScheduler drains the fire, not here — so it isn't a
// parameter; the firing's timestamp is stamped at drain.)
var startBashProbe = func(s *service, fleetName string, trigger fleet.Trigger) {
	go func() {
		// Acquire a concurrency slot, but bail promptly if the daemon is shutting
		// down rather than waiting for a slot first.
		select {
		case bashProbeSem <- struct{}{}:
		case <-s.bgCtx.Done():
			return
		}
		defer func() { <-bashProbeSem }()

		ctx, cancel := context.WithTimeout(s.bgCtx, bashProbeTimeout)
		defer cancel()
		stdout, ok, err := runBashScript(ctx, trigger.Script)
		if !ok {
			if probeFailureIsAlarming(ctx, err) {
				flog.Warn("automation: bash trigger probe failed", "fleet", fleetName, "trigger", trigger.Name, "err", err)
			}
			return
		}
		flog.Info("automation: bash trigger fired", "fleet", fleetName, "trigger", trigger.Name, "bytes", len(stdout))
		// Deliver as a single-fire batch on the shared channel (drained by
		// runScheduler → fireTriggerBatch on the scheduler goroutine). Best-effort:
		// give up if the daemon is shutting down rather than leaking this goroutine;
		// the trigger polls again when the next daemon starts.
		if !s.handFires(s.bgCtx, []triggerFire{{fleet: fleetName, trigger: trigger, body: stdout}}, 0) {
			flog.Warn("automation: bash trigger fired as the daemon stopped; its agents were not started", "fleet", fleetName, "trigger", trigger.Name)
		}
	}()
}

// probeFailureIsAlarming reports whether a non-firing probe outcome deserves a
// warning. A clean non-zero exit (the polled condition simply isn't met) is the
// normal steady state of a frequent poll, so it stays silent — logging it would
// spam a line every tick. But a TIMEOUT is alarming, and it must be detected via
// the context deadline, NOT the error type: a timed-out command is SIGKILLed and
// so ALSO surfaces as an *exec.ExitError ("signal: killed"), indistinguishable
// from a clean non-zero exit by type alone. Any non-ExitError failure (couldn't
// exec, or WaitDelay force-closed the pipes) is alarming too. A shutdown-driven
// cancel (ctx.Err() == context.Canceled, not DeadlineExceeded) stays quiet — the
// daemon is going down, not the script misbehaving.
func probeFailureIsAlarming(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var exitErr *exec.ExitError
	return !errors.As(err, &exitErr)
}

// runBashScript runs a bash trigger's command on the fleet host and returns its
// stdout (the event payload), whether it exited 0, and the run error if any.
// stdout is captured in ISOLATION from stderr, so the payload is exactly the
// command's output; stderr is folded into the error for logging a failed probe.
// The command is killed when ctx (a bashProbeTimeout deadline) expires. A package
// var so tests can stub the exec without spawning a real shell.
//
// WaitDelay is load-bearing: CommandContext SIGKILLs only the direct child
// (bash), but a backgrounded grandchild (e.g. `slow &`) or a pipeline element can
// inherit the stdout pipe's write end and keep it open, so cmd.Run() would block
// at the pipe — well past the deadline — and (worse) then report a zero exit, a
// false fire. WaitDelay force-closes the pipes shortly after the deadline so Run
// returns with an error (→ exitZero false, not-fired) and the goroutine + its
// bashProbeSem slot unwind. Same fix as runProbeWithTimeout (pr_status.go) and
// CombinedOutputWithTimeout (backend/cmd.go).
var runBashScript = func(ctx context.Context, script string) (stdout []byte, exitZero bool, err error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.WaitDelay = 3 * time.Second
	// Bound the captured output so a runaway script can't OOM the daemon. Writes
	// past the cap are discarded, so the command still drains its pipe and runs to
	// completion (the exit code, not the output length, decides whether it fires).
	// stdout is the payload (1 MiB cap); stderr only feeds a failure log, so it
	// gets a much smaller cap — enough to diagnose, never enough to bloat a log line.
	out := &cappedBuffer{limit: maxBashOutputSize}
	errBuf := &cappedBuffer{limit: maxBashStderrSize}
	cmd.Stdout = out
	cmd.Stderr = errBuf
	err = cmd.Run()
	if err != nil && errBuf.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(errBuf.Bytes())))
	}
	return out.Bytes(), err == nil, err
}

// cappedBuffer is an io.Writer that retains only the first `limit` bytes written
// and silently discards the rest — Write always reports the full length consumed,
// so the writer never backpressures the command's stdout/stderr pipe (it keeps
// draining and runs to completion). It bounds a bash probe's captured output so a
// runaway script can't exhaust memory, the same role maxWebhookBodySize plays for
// webhook bodies.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }
func (c *cappedBuffer) Len() int      { return c.buf.Len() }
