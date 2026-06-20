package server

import (
	"context"
	"fmt"
	"strings"
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
)

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

// schedulerTick fires due triggers and services watched agents for one tick.
func (s *service) schedulerTick(ctx context.Context, sched *scheduler, now time.Time) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("automation: load state failed", "err", err)
		return
	}
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
	var out []scheduledFire
	for name, f := range fleets {
		if f == nil {
			continue
		}
		for _, t := range f.Settings.Triggers {
			if t.Type != fleet.TriggerSchedule || t.Cron == "" {
				continue
			}
			sched, err := fleet.ParseCron(t.Cron)
			if err != nil || !sched.Matches(now) {
				continue
			}
			key := name + "\x00" + t.Name
			if last, ok := lastFired[key]; ok && !last.Before(minute) {
				continue // already fired this minute
			}
			lastFired[key] = minute
			out = append(out, scheduledFire{fleet: name, trigger: t})
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
// reap it once idle (tmux mode only), and drop it once gone.
func (s *service) serviceWatched(ctx context.Context, sched *scheduler, st *state.State, now time.Time) {
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

		activity, ok := automationActivity(s, w, inst, now)
		if !ok {
			continue // transient capture failure — don't reset activity or reap
		}
		if activity == agentdetect.StateWorking {
			w.lastActive = now
			continue
		}
		if now.Sub(w.lastActive) >= automationIdleTimeout {
			flog.Info("automation: reaping idle agent", "fleet", w.fleet, "instance", w.instance, "idle", now.Sub(w.lastActive).Round(time.Second).String())
			reapAutomationInstance(s, w.fleet, w.instance)
			delete(sched.watched, k)
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
// a detached background process (non-tmux). ${PROMPT}/${SYS_PROMPT} are
// substituted first.
var launchAutomationCommand = func(ctx context.Context, s *service, w *watchedAgent, inst *fleet.Instance) {
	command := fleet.SubstituteAgentCommand(w.command, w.prompt, w.systemPrompt)
	if w.tmux {
		session := tui.ResolveSessionName(inst.Name, "agent")
		spawn := dotfiles.TmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s`, dotfiles.ShQuote(session))
		if out, err := runContainerShell(ctx, inst, spawn); err != nil {
			flog.Error("automation: spawn session failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
			return
		}
		send := fmt.Sprintf(`tmux send-keys -t %s %s Enter`, dotfiles.ShQuote(session), dotfiles.ShQuote(command))
		if out, err := runContainerShell(ctx, inst, send); err != nil {
			flog.Error("automation: send command failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
		}
		return
	}
	// Non-tmux: run detached so the scheduler tick never blocks on the agent.
	detached := fmt.Sprintf(`nohup sh -lc %s >/tmp/fleet-automation.log 2>&1 &`, dotfiles.ShQuote(command))
	if out, err := runContainerShell(ctx, inst, detached); err != nil {
		flog.Error("automation: launch command failed", "instance", inst.Name, "err", err, "out", strings.TrimSpace(out))
	}
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
