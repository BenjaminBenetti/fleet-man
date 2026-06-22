package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/tui"
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
// launch to idle reaping.
type watchedAgent struct {
	fleet        string
	instance     string
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
}

func (w *watchedAgent) key() string { return w.fleet + "/" + w.instance }

// scheduler holds the loop's per-process state (single-goroutine, no locks).
type scheduler struct {
	lastFired map[string]time.Time     // "fleet\x00trigger" -> minute it last fired
	watched   map[string]*watchedAgent // "fleet/instance" -> tracked agent
}

func newScheduler() *scheduler {
	return &scheduler{
		lastFired: make(map[string]time.Time),
		watched:   make(map[string]*watchedAgent),
	}
}

// webhookFireBuffer is how many matched webhook events may queue between
// scheduler drains before the receiver starts shedding (503). The scheduler
// drains one per loop turn, so this only fills under a sustained burst.
const webhookFireBuffer = 64

// webhookFire is a matched automation webhook event handed from the (concurrent)
// webhook receiver to the scheduler goroutine, which spawns the trigger's agents.
// The receiver has already evaluated the trigger's filter against the event body;
// only the static trigger Prompt (not the body) feeds the agents, matching the
// schedule path.
type webhookFire struct {
	fleet   string
	trigger fleet.Trigger
}

// runScheduler is the automation loop; it returns when ctx is cancelled. It
// ticks once immediately so a trigger whose cron matches the current minute
// fires promptly on daemon start instead of waiting up to a full interval, and
// drains webhook fires between ticks so a delivered event spawns its agents
// promptly. Both paths run on THIS goroutine, so sched's maps stay lock-free.
func (s *service) runScheduler(ctx context.Context) {
	sched := newScheduler()
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	for {
		s.schedulerTick(ctx, sched, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case batch := <-s.webhookFires:
			s.fireWebhookBatch(sched, batch, time.Now())
		}
	}
}

// fireWebhookBatch spawns the agents of every matched webhook trigger in a
// request's batch, on the SCHEDULER goroutine (so it touches sched.watched
// without locking, like the schedule path). The receiver already matched each
// trigger's filter; here we just resolve each fleet's CURRENT agents (state may
// have changed since delivery) and spawn them. The next loop iteration's
// schedulerTick reloads state and services the freshly-watched agents (launch on
// running, reap on idle), so webhook-spawned agents ride the exact same lifecycle
// as scheduled ones.
func (s *service) fireWebhookBatch(sched *scheduler, batch []webhookFire, now time.Time) {
	if len(batch) == 0 {
		return
	}
	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("automation: webhook load state failed", "err", err)
		return
	}
	for _, f := range batch {
		agents := agentsForTrigger(st.Fleets[f.fleet], f.trigger)
		if len(agents) == 0 {
			flog.Warn("automation: webhook trigger has no live agents", "fleet", f.fleet, "trigger", f.trigger.Name)
			continue
		}
		s.fireTriggerAgents(sched, f.fleet, f.trigger, agents, now)
	}
}

// scheduleLoadState loads the persisted state for the scheduler. It is a seam so
// tests can drive schedulerTick deterministically.
var scheduleLoadState = state.Load

// schedulerTick fires due triggers and services watched agents for one tick.
func (s *service) schedulerTick(ctx context.Context, sched *scheduler, now time.Time) {
	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("automation: load state failed", "err", err)
		return
	}
	fired := false
	for _, due := range dueSchedules(st.Fleets, now, sched.lastFired) {
		agents := agentsForTrigger(st.Fleets[due.fleet], due.trigger)
		if s.fireTriggerAgents(sched, due.fleet, due.trigger, agents, now) {
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
}

// fireTriggerAgents spawns one instance per agent the trigger activates and
// registers each in the watch set, returning whether anything was spawned (the
// caller reloads state when so, see schedulerTick). Shared by the schedule path
// and the webhook path; it only mutates sched.watched, so it MUST run on the
// scheduler goroutine.
func (s *service) fireTriggerAgents(sched *scheduler, fleetName string, trigger fleet.Trigger, agents []fleet.Agent, now time.Time) bool {
	fired := false
	for _, ag := range agents {
		instName, err := createAutomationInstance(s, fleetName, ag, now)
		if err != nil {
			flog.Warn("automation: create instance failed", "fleet", fleetName, "agent", ag.Name, "err", err)
			continue
		}
		flog.Info("automation: trigger fired", "fleet", fleetName, "trigger", trigger.Name, "agent", ag.Name, "instance", instName)
		w := &watchedAgent{
			fleet:        fleetName,
			instance:     instName,
			command:      ag.Command,
			prompt:       trigger.Prompt,
			systemPrompt: ag.SystemPrompt,
			spawnedAt:    now,
			lastActive:   now,
		}
		sched.watched[w.key()] = w
		fired = true
	}
	return fired
}

// scheduledFire is one trigger that should fire now.
type scheduledFire struct {
	fleet   string
	trigger fleet.Trigger
}

// dueSchedules returns the schedule triggers whose cron matches now and that
// have not already fired this minute, recording the fire in lastFired. Pure
// except for the lastFired bookkeeping, so it is unit-tested directly.
func dueSchedules(fleets map[string]*fleet.Fleet, now time.Time, lastFired map[string]time.Time) []scheduledFire {
	minute := now.Truncate(time.Minute)
	current := make(map[string]struct{})
	var out []scheduledFire
	for name, f := range fleets {
		if f == nil {
			continue
		}
		for _, t := range f.Settings.Triggers {
			if t.Type != fleet.TriggerSchedule {
				continue
			}
			key := name + "\x00" + t.Name
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
			delete(sched.watched, k) // instance gone (reaped / destroyed)
			continue
		}
		switch inst.Status {
		case fleet.StatusFailed:
			// Provisioning failed; stop tracking and leave the record for the user.
			delete(sched.watched, k)
			continue
		case fleet.StatusRunning:
			// fall through
		default:
			continue // still creating / transitional
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
		if now.Sub(c.w.lastActive) >= automationIdleTimeout {
			flog.Info("automation: reaping idle agent", "fleet", c.w.fleet, "instance", c.w.instance, "idle", now.Sub(c.w.lastActive).Round(time.Second).String())
			reapAutomationInstance(s, c.w.fleet, c.w.instance)
			delete(sched.watched, c.key)
		}
	}
}

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
// its name. It reuses the same job path as the CreateInstance RPC.
var createAutomationInstance = func(s *service, fleetName string, ag fleet.Agent, now time.Time) (string, error) {
	instName := automationInstanceName(ag.Name, now)
	if _, err := s.startCreateInstanceJob(&fleetgrpc.CreateInstanceRequest{
		Fleet:    fleetName,
		Instance: instName,
		Backend:  backendToProto(ag.Backend),
	}, true); err != nil {
		return "", err
	}
	return instName, nil
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
	command := fleet.SubstituteAgentCommand(w.command, w.prompt, w.systemPrompt)
	script := buildAgentLaunchScript(session, command)
	b := s.hub.backendFor(inst)
	go func() {
		if out, err := b.RunScript(inst.ContainerID, script); err != nil {
			flog.Error("automation: launch agent failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
		}
	}()
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
	return w.detector.Detect(caps, now), true
}

// reapAutomationInstance tears down an idle automation instance (container +
// record) via the same destroy job the CLI/TUI use.
var reapAutomationInstance = func(s *service, fleetName, instanceName string) {
	if _, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{
		Fleet:    fleetName,
		Instance: &instanceName,
	}); err != nil {
		flog.Error("automation: reap failed", "fleet", fleetName, "instance", instanceName, "err", err)
	}
}
