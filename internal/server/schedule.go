package server

import (
	"bytes"
	"context"
	"encoding/base64"
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
	// event describes the trigger firing that spawned this agent. At launch its
	// payload is written into the instance and a note pointing at that file is
	// appended to the prompt, so the agent knows what fired it. Nil only if a
	// future caller spawns an agent outside the trigger path.
	event *triggerEvent
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
// the fire time for a schedule trigger (which carries no external payload).
func (e *triggerEvent) payload() []byte {
	if e.kind == fleet.TriggerSchedule {
		return []byte(e.firedAt.UTC().Format(time.RFC3339) + "\n")
	}
	return e.body
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

// maxBashOutputSize caps how many bytes of a probe's stdout/stderr are retained.
// The stdout becomes the event payload (copied into the agent's instance and
// persisted to the trigger log), so an unbounded capture would let a runaway
// script (`yes`, a huge `curl`) OOM the daemon. Mirrors the webhook path's
// maxWebhookBodySize bound; output past it is discarded (see cappedBuffer).
const maxBashOutputSize = 1 << 20

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
		case batch := <-s.triggerFires:
			s.fireTriggerBatch(sched, batch, time.Now())
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
func (s *service) fireTriggerBatch(sched *scheduler, batch []triggerFire, now time.Time) {
	if len(batch) == 0 {
		return
	}
	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("automation: fire batch load state failed", "err", err)
		return
	}
	for _, f := range batch {
		agents := agentsForTrigger(st.Fleets[f.fleet], f.trigger)
		if len(agents) == 0 {
			flog.Warn("automation: fired trigger has no live agents", "fleet", f.fleet, "trigger", f.trigger.Name)
			continue
		}
		s.fireTriggerAgents(sched, f.fleet, f.trigger, agents, now, f.body)
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
	for _, due := range dueCronTriggers(st.Fleets, now, sched.lastFired) {
		if due.trigger.Type == fleet.TriggerBash {
			// A bash trigger's cron only schedules a poll — it fires its agents only
			// if its command exits 0. Run the command OFF the tick (it may be slow or
			// block); on a zero exit the probe hands the trigger back via triggerFires
			// to spawn the agents (see startBashProbe). Nothing is spawned here, so
			// `fired` stays as-is.
			startBashProbe(s, due.fleet, due.trigger)
			continue
		}
		agents := agentsForTrigger(st.Fleets[due.fleet], due.trigger)
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
}

// fireTriggerAgents spawns one instance per agent the trigger activates and
// registers each in the watch set, returning whether anything was spawned (the
// caller reloads state when so, see schedulerTick). body is the webhook request
// payload (nil for schedule triggers); it rides each watched agent's event so
// the launch can hand it to the agent. Shared by the schedule path and the
// webhook path; it only mutates sched.watched, so it MUST run on the scheduler
// goroutine.
func (s *service) fireTriggerAgents(sched *scheduler, fleetName string, trigger fleet.Trigger, agents []fleet.Agent, now time.Time, body []byte) bool {
	// One event describes this firing; it's shared (read-only) by every agent the
	// trigger spawns and recorded once to the trigger's on-host event log so the
	// firing is debuggable after the instances are reaped (see trigger_logs.go).
	ev := &triggerEvent{
		kind:        trigger.Type,
		triggerName: trigger.Name,
		firedAt:     now,
		webhookName: trigger.WebhookName,
		body:        body,
	}
	logTriggerEvent(fleetName, ev)
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
			event:        ev,
		}
		sched.watched[w.key()] = w
		fired = true
	}
	return fired
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
			if err := writeAutomationEventFile(inst, eventPath, w.event.payload()); err != nil {
				flog.Warn("automation: write event file failed", "instance", inst.Name, "path", eventPath, "err", err)
			}
		}
		if out, err := b.RunScript(inst.ContainerID, script); err != nil {
			flog.Error("automation: launch agent failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
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

// writeAutomationEventFile writes data to path inside the instance. It reuses the
// fleet-copy exec seam (copyIntoExecCommand) and streams the payload as base64
// over STDIN, decoded in-container — so a payload of any size avoids the argv
// length limit a shell-embedded write would hit, arbitrary bytes survive intact,
// and the base64 text stays clean over the codespaces backend's exec PTY (which
// mangles raw binary). A package var so tests can stub the exec.
var writeAutomationEventFile = func(inst *fleet.Instance, path string, data []byte) error {
	cmd := copyIntoExecCommand(inst, []string{"sh", "-c", "base64 -d > " + dotfiles.ShQuote(path)})
	cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
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
			// A clean non-zero exit just means the polled condition isn't met — the
			// normal steady state for a frequent poll, so stay silent (logging it would
			// spam a line every tick). An exec/timeout failure (not an *exec.ExitError)
			// is a real problem worth surfacing.
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				flog.Warn("automation: bash trigger probe failed", "fleet", fleetName, "trigger", trigger.Name, "err", err)
			}
			return
		}
		flog.Info("automation: bash trigger fired", "fleet", fleetName, "trigger", trigger.Name, "bytes", len(stdout))
		// Deliver as a single-fire batch on the shared channel (drained by
		// runScheduler → fireTriggerBatch on the scheduler goroutine). Best-effort:
		// give up if the daemon is shutting down rather than leaking this goroutine.
		select {
		case s.triggerFires <- []triggerFire{{fleet: fleetName, trigger: trigger, body: stdout}}:
		case <-s.bgCtx.Done():
		}
	}()
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
	out := &cappedBuffer{limit: maxBashOutputSize}
	errBuf := &cappedBuffer{limit: maxBashOutputSize}
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
