package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// automation_watch.go persists the scheduler's watch list across daemon
// restarts. The list (the automation runs the scheduler spawned and still has
// to launch or reap) used to live only in memory, so a restart — an upgrade, a
// crash — left every running automation instance unreaped for good. Each
// change is now written to ~/.fleet/automation/watch.json (atomicfile, 0600),
// together with the minute each cron trigger last fired, and the scheduler
// reloads it on start:
//
//   - a launched run resumes idle reaping, its idle clock restarted;
//   - an unlaunched run launches normally once its instance runs, with the
//     command and prompts it was fired with and its payload read back from the
//     firing's trigger event log;
//   - a run whose instance is gone is dropped, and one whose create the restart
//     interrupted is FAILED by then (interrupted_jobs.go), so the first tick
//     drops it and leaves the record for the user, as for any failed run;
//   - an Automated instance missing from the file (spawned by a daemon that
//     predates the file, or one whose write failed) is adopted: reaped on the
//     idle rule, or, while it has no agent session, once idle for
//     automationAdoptGrace;
//   - a cron trigger that already fired this minute does not fire again.

// automationAdoptGrace is how long an adopted run with no agent session may
// stay idle before it is reaped. Longer than the idle timeout: without the
// agent's session nothing says the run is still wanted, but someone may be
// using the instance by hand. Overridable via FLEET_AUTOMATION_ADOPT_GRACE.
var automationAdoptGrace = envDurationDefault("FLEET_AUTOMATION_ADOPT_GRACE", 10*time.Minute)

// watchFileVersion versions watch.json. A file of another version is ignored
// (with a warning) rather than misread.
const watchFileVersion = 1

type watchFile struct {
	Version int          `json:"v"`
	Runs    []watchRun   `json:"runs"`
	Fired   []watchFired `json:"fired,omitempty"`
}

// watchRun is one persisted watchedAgent.
type watchRun struct {
	Fleet        string         `json:"fleet"`
	Instance     string         `json:"instance"`
	Trigger      string         `json:"trigger,omitempty"`
	Agent        string         `json:"agent,omitempty"`
	Command      string         `json:"command,omitempty"`
	Prompt       string         `json:"prompt,omitempty"`
	SystemPrompt string         `json:"systemPrompt,omitempty"`
	SpawnedAt    time.Time      `json:"spawnedAt"`
	Launched     bool           `json:"launched,omitempty"`
	Adopted      bool           `json:"adopted,omitempty"`
	Event        *watchRunEvent `json:"event,omitempty"`
}

// watchRunEvent is the firing a run was spawned for. The payload itself stays
// in the trigger's event log (LogFile, a name in its log directory); PayloadSize
// is the body length, so it reads back exactly.
type watchRunEvent struct {
	Kind        fleet.TriggerType `json:"kind"`
	FiredAt     time.Time         `json:"firedAt"`
	Webhook     string            `json:"webhook,omitempty"`
	LogFile     string            `json:"logFile,omitempty"`
	PayloadSize int               `json:"payloadSize,omitempty"`
}

// watchFired is one lastFired entry: the minute a cron trigger last fired.
type watchFired struct {
	Fleet   string    `json:"fleet"`
	Trigger string    `json:"trigger"`
	Minute  time.Time `json:"minute"`
}

// firedKey is the scheduler's lastFired key for a fleet's trigger.
func firedKey(fleetName, triggerName string) string { return fleetName + "\x00" + triggerName }

// snapshot renders the scheduler's watch list and lastFired as watch.json
// content, sorted so an unchanged list renders identically.
func (sched *scheduler) snapshot() ([]byte, error) {
	wf := watchFile{Version: watchFileVersion, Runs: []watchRun{}}
	for _, w := range sched.watched {
		r := watchRun{
			Fleet:        w.fleet,
			Instance:     w.instance,
			Agent:        w.agent,
			Command:      w.command,
			Prompt:       w.prompt,
			SystemPrompt: w.systemPrompt,
			SpawnedAt:    w.spawnedAt,
			Launched:     w.launched,
			Adopted:      w.adopted,
		}
		if ev := w.event; ev != nil {
			r.Trigger = ev.triggerName
			r.Event = &watchRunEvent{
				Kind:        ev.kind,
				FiredAt:     ev.firedAt,
				Webhook:     ev.webhookName,
				LogFile:     w.eventLog,
				PayloadSize: len(ev.body),
			}
		}
		wf.Runs = append(wf.Runs, r)
	}
	sort.Slice(wf.Runs, func(i, j int) bool {
		if wf.Runs[i].Fleet != wf.Runs[j].Fleet {
			return wf.Runs[i].Fleet < wf.Runs[j].Fleet
		}
		return wf.Runs[i].Instance < wf.Runs[j].Instance
	})
	for key, minute := range sched.lastFired {
		fleetName, triggerName, ok := splitFiredKey(key)
		if !ok {
			continue
		}
		wf.Fired = append(wf.Fired, watchFired{Fleet: fleetName, Trigger: triggerName, Minute: minute})
	}
	sort.Slice(wf.Fired, func(i, j int) bool {
		if wf.Fired[i].Fleet != wf.Fired[j].Fleet {
			return wf.Fired[i].Fleet < wf.Fired[j].Fleet
		}
		return wf.Fired[i].Trigger < wf.Fired[j].Trigger
	})
	return json.MarshalIndent(wf, "", "  ")
}

func splitFiredKey(key string) (fleetName, triggerName string, ok bool) {
	return strings.Cut(key, "\x00")
}

// persist writes the watch list when it changed since the last write. A
// scheduler without a watchPath (tests) keeps it in memory only. Best-effort:
// a failed write is warned and retried on the next change or tick.
func (sched *scheduler) persist() {
	if sched.watchPath == "" {
		return
	}
	data, err := sched.snapshot()
	if err != nil {
		flog.Warn("automation: encode watch list failed", "err", err)
		return
	}
	if sched.saved != nil && bytes.Equal(data, sched.saved) {
		return
	}
	if err := writeWatchFile(sched.watchPath, data); err != nil {
		flog.Warn("automation: save watch list failed", "path", sched.watchPath, "err", err)
		return
	}
	sched.saved = data
}

func writeWatchFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}

// readWatchFile loads watch.json. A missing file is an empty list (and
// existed false).
func readWatchFile(path string) (wf *watchFile, existed bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &watchFile{Version: watchFileVersion}, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	wf = &watchFile{}
	if err := json.Unmarshal(data, wf); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if wf.Version != watchFileVersion {
		return nil, true, fmt.Errorf("%s: unsupported version %d", path, wf.Version)
	}
	return wf, true, nil
}

// restore loads the persisted watch list into sched (see the file comment for
// what each kind of entry becomes) and writes the result back. Runs on the
// scheduler goroutine before its first tick.
func (sched *scheduler) restore(now time.Time) {
	if sched.watchPath == "" {
		return
	}
	wf, existed, err := readWatchFile(sched.watchPath)
	if err != nil {
		flog.Warn("automation: load watch list failed; starting empty", "err", err)
		wf = &watchFile{Version: watchFileVersion}
	}
	if !existed {
		// Nothing to write back unless adoption finds runs: a daemon that never
		// ran automation does not grow the file.
		sched.saved, _ = sched.snapshot()
	}
	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("automation: load state for the watch list failed", "err", err)
		st = nil
	}

	restored, dropped := 0, 0
	for _, r := range wf.Runs {
		if st != nil && findInstance(st, r.Fleet, r.Instance) == nil {
			dropped++
			continue
		}
		w := &watchedAgent{
			fleet:        r.Fleet,
			instance:     r.Instance,
			agent:        r.Agent,
			command:      r.Command,
			prompt:       r.Prompt,
			systemPrompt: r.SystemPrompt,
			spawnedAt:    r.SpawnedAt,
			lastActive:   now, // the idle clock restarts with the daemon
			launched:     r.Launched,
			adopted:      r.Adopted,
		}
		if r.Event != nil {
			w.event = &triggerEvent{
				kind:        r.Event.Kind,
				triggerName: r.Trigger,
				firedAt:     r.Event.FiredAt,
				webhookName: r.Event.Webhook,
			}
			w.eventLog = r.Event.LogFile
			// Only a run still to launch needs its payload back.
			if !r.Launched && r.Event.PayloadSize > 0 {
				body, err := readTriggerEventBody(r.Fleet, w.event, r.Event.LogFile, r.Event.PayloadSize)
				if err != nil {
					flog.Warn("automation: restored run lost its event payload", "fleet", r.Fleet, "instance", r.Instance, "trigger", r.Trigger, "err", err)
				} else {
					w.event.body = body
				}
			}
		}
		sched.watched[w.key()] = w
		restored++
	}
	for _, f := range wf.Fired {
		sched.lastFired[firedKey(f.Fleet, f.Trigger)] = f.Minute
	}

	adopted := 0
	if st != nil {
		for fleetName, f := range st.Fleets {
			if f == nil {
				continue
			}
			for _, inst := range f.Instances {
				if !inst.Automated || inst.Status == fleet.StatusFailed {
					continue
				}
				w := &watchedAgent{
					fleet:      fleetName,
					instance:   inst.Name,
					spawnedAt:  inst.CreatedAt,
					lastActive: now,
					launched:   true, // never launch an agent twice
					adopted:    true,
				}
				if _, ok := sched.watched[w.key()]; ok {
					continue
				}
				sched.watched[w.key()] = w
				adopted++
				flog.Info("automation: adopted an unwatched automation instance", "fleet", fleetName, "instance", inst.Name, "status", inst.Status)
			}
		}
	}
	if restored+dropped+adopted > 0 {
		flog.Info("automation: watch list restored", "runs", restored, "dropped", dropped, "adopted", adopted)
	}
	sched.persist()
}

// idleLimit is how long w may stay idle before it is reaped: the idle timeout,
// or for an adopted run whose agent session is gone, automationAdoptGrace.
func (w *watchedAgent) idleLimit() time.Duration {
	if w.adopted && !w.hasAgentSession && automationAdoptGrace > automationIdleTimeout {
		return automationAdoptGrace
	}
	return automationIdleTimeout
}
