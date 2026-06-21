package server

import (
	"context"
	"fmt"
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
//  2. once that instance is running, launches the agent's command — in a tmux
//     session (default) so the user can open it in the TUI, with the trigger
//     prompt + agent system prompt substituted into ${PROMPT}/${SYS_PROMPT};
//  3. reaps tmux-mode agents that go idle for longer than the idle timeout,
//     detected with the same screen-diff mechanism the TUI shows.
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
	// cron minute at least once while staying cheap.
	scheduleTickInterval = 30 * time.Second
	// automationIdleTimeout is how long a tmux-mode agent may stay inactive
	// before its instance is torn down (issue #188: "inactive for more than 2
	// minutes").
	automationIdleTimeout = 2 * time.Minute
	// automationPromptDelay is how long to wait after starting a tmux-mode
	// agent before typing the prompt into it, giving the agent's REPL time to
	// come up so the keystrokes are not dropped. Best-effort (a fixed delay is
	// imperfect, but agents like Claude Code start within a few seconds).
	automationPromptDelay = 5 * time.Second
)

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
	tmux         bool
	spawnedAt    time.Time
	lastActive   time.Time
	launched     bool
	detector     *agentdetect.TmuxPaneChangeDetector
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

// runScheduler is the automation loop; it returns when ctx is cancelled.
func (s *service) runScheduler(ctx context.Context) {
	sched := newScheduler()
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.schedulerTick(ctx, sched, time.Now())
		}
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
		for _, ag := range agentsForTrigger(st.Fleets[due.fleet], due.trigger) {
			instName, err := createAutomationInstance(s, due.fleet, ag, now)
			if err != nil {
				flog.Warn("automation: create instance failed", "fleet", due.fleet, "agent", ag.Name, "err", err)
				continue
			}
			flog.Info("automation: trigger fired", "fleet", due.fleet, "trigger", due.trigger.Name, "agent", ag.Name, "instance", instName)
			w := &watchedAgent{
				fleet:        due.fleet,
				instance:     instName,
				command:      ag.Command,
				prompt:       due.trigger.Prompt,
				systemPrompt: ag.SystemPrompt,
				tmux:         ag.TmuxMode,
				spawnedAt:    now,
				lastActive:   now,
				detector:     agentdetect.NewTmuxPaneChangeDetector(),
			}
			sched.watched[w.key()] = w
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
			// Non-tmux agents are fire-and-forget: once launched there is no idle
			// state to watch, so stop tracking them (otherwise the watch set would
			// grow without bound).
			if !w.tmux {
				delete(sched.watched, k)
			}
			continue
		}
		if !w.tmux {
			// A non-tmux agent is dropped at launch above; never idle-reap one.
			delete(sched.watched, k)
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
	}); err != nil {
		return "", err
	}
	return instName, nil
}

// launchAutomationCommand runs the agent's command inside its (running)
// instance: a detached tmux session the user can open in the TUI (tmux mode), or
// a detached background process (non-tmux).
//
// In tmux mode the prompt is delivered the way issue #188 specifies — "the
// PROMPT will be sent via tmux send keys": the agent command (with ${SYS_PROMPT}
// substituted) starts the agent, then the trigger prompt is typed into it as a
// SEPARATE send-keys line. That is what makes the default command —
// `claude --system-prompt '${SYS_PROMPT}'`, which has no ${PROMPT} — actually do
// work: the prompt is typed into the running agent rather than relying on a
// ${PROMPT} placeholder the default doesn't contain. If the command DOES embed
// ${PROMPT} the prompt is substituted there instead and not re-sent.
//
// The tmux script runs in a goroutine because it sleeps (giving the agent's REPL
// a moment to start before the prompt is typed) and must not stall the tick.
var launchAutomationCommand = func(ctx context.Context, s *service, w *watchedAgent, inst *fleet.Instance) {
	// Exec straight against the container (RunScript), NOT the devcontainer Node
	// CLI path (ExecCommand/runContainerShell): from the daemon's scheduler the
	// Node CLI path silently produced no tmux server, whereas RunScript runs as
	// the same session user the poller reads, so the agent's session reliably
	// comes up and shows in the TUI.
	b := s.hub.backendFor(inst)
	if w.tmux {
		session := tui.ResolveSessionName(inst.Name, "agent")
		cmdHasPrompt := strings.Contains(w.command, "${PROMPT}")
		command := fleet.SubstituteAgentCommand(w.command, w.prompt, w.systemPrompt)
		script := buildTmuxLaunchScript(session, command, w.prompt, !cmdHasPrompt)
		// The script sleeps to await REPL readiness before typing the prompt, so
		// run it off the tick.
		go func() {
			if out, err := b.RunScript(inst.ContainerID, script); err != nil {
				flog.Error("automation: launch tmux agent failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
			}
		}()
		return
	}
	// Non-tmux: run detached so the launch never blocks. There is no interactive
	// session to type into, so a non-tmux agent must use the ${PROMPT} placeholder
	// in its command to receive the prompt.
	command := fleet.SubstituteAgentCommand(w.command, w.prompt, w.systemPrompt)
	detached := fmt.Sprintf(`nohup sh -lc %s >/tmp/fleet-automation.log 2>&1 &`, dotfiles.ShQuote(command))
	if out, err := b.RunScript(inst.ContainerID, detached); err != nil {
		flog.Error("automation: launch command failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
	}
}

// buildTmuxLaunchScript builds the in-container shell snippet that starts a
// tmux-mode agent: ensure tmux, create the (detached) session, type the agent
// command, then — when sendPrompt is set — wait for the agent's REPL to come up
// and type the prompt into it. Text is sent with `send-keys -l` (literal) so a
// prompt that happens to contain tmux key names ("Enter", "C-c", ...) is typed
// verbatim, with a separate bare `Enter` to submit each line.
func buildTmuxLaunchScript(sessionName, agentCommand, prompt string, sendPrompt bool) string {
	sess := dotfiles.ShQuote(sessionName)
	var b strings.Builder
	b.WriteString(dotfiles.TmuxEnsureInstalled)
	fmt.Fprintf(&b, "tmux new-session -d -s %s\n", sess)
	fmt.Fprintf(&b, "tmux send-keys -t %s -l -- %s\n", sess, dotfiles.ShQuote(agentCommand))
	fmt.Fprintf(&b, "tmux send-keys -t %s Enter\n", sess)
	if sendPrompt && prompt != "" {
		fmt.Fprintf(&b, "sleep %d\n", int(automationPromptDelay.Seconds()))
		fmt.Fprintf(&b, "tmux send-keys -t %s -l -- %s\n", sess, dotfiles.ShQuote(prompt))
		fmt.Fprintf(&b, "tmux send-keys -t %s Enter\n", sess)
	}
	return b.String()
}

// automationActivity reports a watched agent's current activity by diffing its
// tmux screen (the same mechanism the TUI shows). The bool is false on a
// transient capture failure, telling the caller to leave the agent untouched.
var automationActivity = func(s *service, w *watchedAgent, inst *fleet.Instance, now time.Time) (agentdetect.State, bool) {
	if inst.ContainerID == "" {
		return agentdetect.StateNotRunning, false
	}
	caps := s.hub.backendFor(inst).CaptureAllSessions(inst.ContainerID)
	if !caps.OK {
		return agentdetect.StateNotRunning, false
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
