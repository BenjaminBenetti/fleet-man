package tui

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"

	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Constants
// ===========================================

// liveStatusPollInterval controls how often the TUI re-probes live
// state for every instance. Backstop against drift while fleet is
// running; the startup probe handles the dominant overnight case.
const liveStatusPollInterval = time.Minute

// liveStatusStaggerWindow is the wall-clock window across which a
// single cycle's per-instance probes are spread. Slightly shorter
// than liveStatusPollInterval so the slowest probe in a cycle
// completes with margin before the next tick fires, even when the
// fleet is large.
//
// Probes inside a cycle start at offsets 0, slot, 2·slot, … where
// slot = liveStatusStaggerWindow / N. With N=1 the lone probe runs
// immediately; with large N the slot shrinks but never collapses to
// zero, so a many-instance fleet never produces a thundering herd
// against gh / coder / docker.
const liveStatusStaggerWindow = 55 * time.Second

// ===========================================
// Messages
// ===========================================

// liveStatusTickMsg fires every liveStatusPollInterval and triggers
// a fresh probe of every instance.
type liveStatusTickMsg struct{}

// liveStatusMsg carries the result of a probe pass: one entry per
// "fleet/instance" key, with the LiveStatus reported by that instance's
// backend. Missing keys mean the instance was skipped (no container
// ID yet, transient state in flight).
type liveStatusMsg struct {
	updates map[string]backend.LiveStatus
}

// ===========================================
// Probe Targets
// ===========================================

// liveStatusProbe is a self-contained snapshot of everything a
// background goroutine needs to query a single instance's live state
// without holding a reference to the (mutable) model or its backends
// map.
type liveStatusProbe struct {
	fleetName    string
	instanceName string
	backendType  fleet.BackendType
	containerID  string
}

// collectLiveStatusProbes returns the set of instances that are
// candidates for a live-state probe.
//
// Skipped:
//   - instances with no container ID (creation hasn't produced one)
//   - instances in transient states — the backend hasn't finished, or
//     a fleet-driven action is in flight, and we don't want a stale
//     probe to clobber a status we're actively driving
//
// Both running and stopped instances are probed. A stopped codespace
// the user re-enabled via the GitHub UI is just as much a drift case
// as a running container that crashed.
func collectLiveStatusProbes(st *state.State) []liveStatusProbe {
	if st == nil {
		return nil
	}
	var probes []liveStatusProbe
	for fleetName, f := range st.Fleets {
		for _, instance := range f.Instances {
			if instance.ContainerID == "" {
				continue
			}
			switch instance.Status {
			case fleet.StatusRunning, fleet.StatusStopped:
				// fall through
			default:
				continue
			}
			probes = append(probes, liveStatusProbe{
				fleetName:    fleetName,
				instanceName: instance.Name,
				backendType:  instance.Backend,
				containerID:  instance.ContainerID,
			})
		}
	}
	return probes
}

// ===========================================
// Commands
// ===========================================

// liveStatusPollCmd schedules the next periodic refresh tick.
func liveStatusPollCmd() tea.Cmd {
	return tea.Tick(liveStatusPollInterval, func(time.Time) tea.Msg {
		return liveStatusTickMsg{}
	})
}

// refreshLiveStatusCmd issues one Cmd per instance, each delayed so
// the cycle's probes are spread evenly across liveStatusStaggerWindow.
// This keeps gh / coder / docker from being hit by a thundering herd
// at the start of every cycle, and lets each instance's status update
// land independently as soon as its probe returns instead of waiting
// for the slowest one in the batch.
//
// Returns nil when there is nothing to probe so callers can pass the
// result straight into tea.Batch without a guard.
func refreshLiveStatusCmd(probes []liveStatusProbe) tea.Cmd {
	if len(probes) == 0 {
		return nil
	}
	slot := liveStatusStaggerWindow / time.Duration(len(probes))
	cmds := make([]tea.Cmd, len(probes))
	for index, probe := range probes {
		cmds[index] = probeLiveStatusCmd(probe, time.Duration(index)*slot)
	}
	return tea.Batch(cmds...)
}

// probeLiveStatusCmd queries a single instance's backend after the
// requested delay and posts a single-entry liveStatusMsg with the
// result. A fresh Backend is constructed inside the goroutine so
// the shared model.backends map is not touched from off-thread code.
func probeLiveStatusCmd(probe liveStatusProbe, delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		if delay > 0 {
			time.Sleep(delay)
		}
		instanceBackend := backendutil.New(probe.backendType, false)
		return liveStatusMsg{updates: map[string]backend.LiveStatus{
			probe.fleetName + "/" + probe.instanceName: instanceBackend.Status(probe.containerID),
		}}
	}
}

// ===========================================
// Reconciliation
// ===========================================

// applyLiveStatuses reconciles the persisted instance statuses with
// what each backend reports as live state. Returns true when at least
// one instance changed and was saved.
//
// Reconciliation rules:
//
//   - Only running ↔ stopped flips are applied. Transient statuses
//     (creating/starting/stopping/deleting/failed) are left alone —
//     a fleet-driven transition is in flight or has terminally failed,
//     and a probe must not overwrite that.
//
//   - LiveStatusUnknown means the probe was inconclusive (network,
//     auth, daemon down). Persisted state is preserved.
//
//   - LiveStatusMissing is recorded as a warning on the instance but
//     the status is left as-is. The user controls deletion explicitly;
//     fleet should not silently drop instances on a probe glitch that
//     looks like missing-but-isn't.
func applyLiveStatuses(st *state.State, updates map[string]backend.LiveStatus) bool {
	if st == nil || len(updates) == 0 {
		return false
	}
	changed := false
	for _, f := range st.Fleets {
		for _, instance := range f.Instances {
			key := f.Name + "/" + instance.Name
			liveStatus, ok := updates[key]
			if !ok {
				continue
			}
			switch instance.Status {
			case fleet.StatusRunning, fleet.StatusStopped:
				// fall through
			default:
				continue
			}
			var desired fleet.InstanceStatus
			switch liveStatus {
			case backend.LiveStatusRunning:
				desired = fleet.StatusRunning
			case backend.LiveStatusStopped:
				desired = fleet.StatusStopped
			default:
				continue
			}
			if instance.Status != desired {
				instance.Status = desired
				if desired == fleet.StatusRunning {
					instance.Error = ""
				}
				changed = true
			}
		}
	}
	if changed {
		if err := state.Save(st); err != nil {
			return false
		}
	}
	return changed
}
