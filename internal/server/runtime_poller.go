package server

import (
	"context"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Poll cadences mirror the TUI's original pollers (live_status.go ~60s,
// stats/activity ~3s, sessions ~1s). All are GATED on a runtime subscriber
// existing (h.runtimeWanted), so with no TUI connected the server stays quiet —
// `fleet ls` (GetState) never opens Watch, so nothing here runs.
const (
	liveStatusInterval    = time.Minute
	statsActivityInterval = 3 * time.Second
	sessionsPollInterval  = time.Second
)

// backendCacheKey identifies a cached backend. ContainerID alone is not unique
// across backend types (a Coder workspace and a Codespace can share a name), so
// the backend type is part of the key — otherwise two same-named instances on
// different backends would reuse one backend and the poller would drive the
// wrong CLI.
func backendCacheKey(inst *fleet.Instance) string {
	return string(inst.Backend) + "\x00" + inst.ContainerID
}

// backendFor returns a cached Backend for inst, keyed by (backend type,
// container ID), building one on first use. Reusing the instance across poll
// ticks lets the backend's internal caches survive — notably the devcontainer
// container-user lookup, which a fresh-backend-per-tick always missed,
// re-running a `docker exec` probe every pass.
func (h *hub) backendFor(inst *fleet.Instance) backend.Backend {
	key := backendCacheKey(inst)
	h.backendsMu.Lock()
	defer h.backendsMu.Unlock()
	if h.backends == nil {
		h.backends = make(map[string]backend.Backend)
	}
	if b, ok := h.backends[key]; ok {
		return b
	}
	b := backendutil.NewForInstance(inst, false)
	h.backends[key] = b
	return b
}

// pruneBackends drops cached backends whose instance is no longer in the active
// set (stopped, removed, or rebuilt to a new container ID), bounding the cache
// to the currently running instances. Called once per stats tick with the live
// cache keys (see backendCacheKey).
func (h *hub) pruneBackends(activeKeys []string) {
	active := make(map[string]struct{}, len(activeKeys))
	for _, k := range activeKeys {
		active[k] = struct{}{}
	}
	h.backendsMu.Lock()
	defer h.backendsMu.Unlock()
	for k := range h.backends {
		if _, ok := active[k]; !ok {
			delete(h.backends, k)
		}
	}
}

// startRuntimePollers launches the three live-runtime pollers. Each populates
// only its own InstanceRuntime fields and broadcasts changed entries.
func startRuntimePollers(ctx context.Context, h *hub) {
	go liveStatusPoller(ctx, h)
	go statsActivityPoller(ctx, h)
	go sessionsPoller(ctx, h)
}

// runtimeUpdate is one instance's field mutation, applied on the hub loop.
type runtimeUpdate struct {
	fleet    string
	instance string
	apply    func(*fleetgrpc.InstanceRuntime)
}

// applyRuntime merges each update's owned fields into the authoritative
// h.runtime[key] in place, then broadcasts the entries that actually changed.
// UpdatedAt bumps only on a real change (so it doesn't itself trigger a diff).
// Runs on the hub loop.
func (h *hub) applyRuntime(ups []runtimeUpdate) {
	var changed []*fleetgrpc.InstanceRuntime
	for _, u := range ups {
		key := runtimeKey(u.fleet, u.instance)
		r := h.runtime[key]
		if r == nil {
			r = &fleetgrpc.InstanceRuntime{Fleet: u.fleet, Instance: u.instance}
			h.runtime[key] = r
		}
		before := proto.Clone(r).(*fleetgrpc.InstanceRuntime)
		u.apply(r)
		if !proto.Equal(before, r) {
			r.UpdatedAt = timestamppb.Now()
			changed = append(changed, cloneRuntime(r))
		}
	}
	h.broadcastRuntime(changed)
}

// probeLiveStatus reports one instance's backend live status. It is a package
// var so the persisted-status reconciliation in liveStatusPass is unit-testable
// without a real backend.
var probeLiveStatus = func(inst *fleet.Instance) backend.LiveStatus {
	return backendutil.NewForInstance(inst, false).Status(inst.ContainerID)
}

// liveStatusPoller probes each instance's backend live status (running/stopped/
// missing/unknown) for the runtime sidecar AND reconciles the persisted
// running<->stopped status when a conclusive probe diverges from it (e.g. a
// container the user stopped/started outside fleet). Runs on the 60s tick and
// immediately on the false->true gate edge so a freshly-subscribed TUI gets a
// live_status hint without waiting a minute.
func liveStatusPoller(ctx context.Context, h *hub) {
	ticker := time.NewTicker(liveStatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.runtimeEdge:
			if h.runtimeWanted.Load() {
				liveStatusPass(h)
			}
		case <-ticker.C:
			if h.runtimeWanted.Load() {
				liveStatusPass(h)
			}
		}
	}
}

func liveStatusPass(h *hub) {
	st, err := state.Load()
	if err != nil {
		return
	}
	var ups []runtimeUpdate
	// reconcile["fleet/instance"] = desired persisted status, for the conclusive
	// running<->stopped divergences detected this pass.
	reconcile := make(map[string]fleet.InstanceStatus)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.ContainerID == "" {
				continue
			}
			if inst.Status != fleet.StatusRunning && inst.Status != fleet.StatusStopped {
				continue
			}
			live := probeLiveStatus(inst)
			ls := liveStatusToProto(live)
			ups = append(ups, runtimeUpdate{fleetName, inst.Name, func(r *fleetgrpc.InstanceRuntime) {
				r.LiveStatus = ls
			}})
			// Only conclusive running/stopped probes reconcile the persisted
			// status; unknown/missing leave it as-is (matches the legacy TUI rule).
			switch live {
			case backend.LiveStatusRunning:
				if inst.Status != fleet.StatusRunning {
					reconcile[fleetName+"/"+inst.Name] = fleet.StatusRunning
				}
			case backend.LiveStatusStopped:
				if inst.Status != fleet.StatusStopped {
					reconcile[fleetName+"/"+inst.Name] = fleet.StatusStopped
				}
			}
		}
	}
	if len(reconcile) > 0 {
		_ = state.Update(func(st *state.State) error {
			for _, f := range st.Fleets {
				for _, inst := range f.Instances {
					desired, ok := reconcile[f.Name+"/"+inst.Name]
					if !ok {
						continue
					}
					// Re-check under the lock: skip if a job transitioned it out of
					// running/stopped since the probe.
					if inst.Status != fleet.StatusRunning && inst.Status != fleet.StatusStopped {
						continue
					}
					if inst.Status != desired {
						inst.Status = desired
						if desired == fleet.StatusRunning {
							inst.Error = ""
						}
					}
				}
			}
			return nil
		})
		// Broadcast the reconciled snapshot promptly (the 1s state poller would
		// also catch it, but Watch subscribers shouldn't wait a tick).
		if reloaded, lerr := state.Load(); lerr == nil {
			snapshot := stateToProto(reloaded)
			h.post(func(h *hub) { h.setState(snapshot) })
		}
	}
	if len(ups) > 0 {
		h.post(func(h *hub) { h.applyRuntime(ups) })
	}
}

// statsActivityPoller fills CPU/mem stats, agent tool/activity, and the resolved
// git branch for running instances every ~3s.
func statsActivityPoller(ctx context.Context, h *hub) {
	ticker := time.NewTicker(statsActivityInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.runtimeWanted.Load() {
				statsActivityPass(h)
			}
		}
	}
}

func statsActivityPass(h *hub) {
	st, err := state.Load()
	if err != nil {
		return
	}
	type item struct {
		fleetName, instance, containerID, workspaceDir string
		inst                                           *fleet.Instance
	}
	var items []item
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.ContainerID == "" || inst.Status != fleet.StatusRunning {
				continue
			}
			items = append(items, item{fleetName, inst.Name, inst.ContainerID, inst.WorkspaceDir, inst})
		}
	}
	if len(items) == 0 {
		return
	}

	statsByID := make(map[string]*backend.ContainerStats, len(items))
	captures := make(map[string]backend.AllSessions, len(items))
	probes := make(map[string]string, len(items))
	branchByKey := make(map[string]string, len(items))
	expectedIDs := make([]string, 0, len(items))

	// Fetch stats for all containers in one call per backend. The devcontainer
	// backend turns this into a single `docker stats --no-stream <id...>`; that
	// call blocks ~1s in dockerd by design, so issuing it per instance
	// serialized the whole pass (N seconds for N instances). One call covers
	// them all. The SSH backends fan out internally regardless.
	statsIDs := make(map[fleet.BackendType][]string)
	statsBackend := make(map[fleet.BackendType]backend.Backend)
	for _, it := range items {
		statsIDs[it.inst.Backend] = append(statsIDs[it.inst.Backend], it.containerID)
		statsBackend[it.inst.Backend] = h.backendFor(it.inst)
	}
	for bt := range statsIDs {
		ids := statsIDs[bt]
		b := statsBackend[bt]
		m, err := b.Stats(ids)
		if err != nil && len(ids) > 1 {
			// `docker stats --no-stream` fails for the WHOLE batch (non-zero
			// exit, no output) if any one requested container was removed since
			// state was read — which would blank stats for every instance this
			// tick. Retry per container, concurrently, so a single casualty is
			// isolated and the rest still report (the SSH backends already
			// isolate internally, so this path is devcontainer-only in practice).
			m, _ = backend.ConcurrentStats(ids, func(id string) (*backend.ContainerStats, error) {
				single, serr := b.Stats([]string{id})
				if serr != nil {
					return nil, serr
				}
				return single[id], nil
			})
		}
		for id, s := range m {
			if s != nil {
				statsByID[id] = s
			}
		}
	}

	activeKeys := make([]string, 0, len(items))
	for _, it := range items {
		b := h.backendFor(it.inst)
		captures[it.containerID] = b.CaptureAllSessions(it.containerID)
		if tool, ok := b.AgentToolProbe(it.containerID); ok {
			probes[it.containerID] = tool
		}
		expectedIDs = append(expectedIDs, it.containerID)
		activeKeys = append(activeKeys, backendCacheKey(it.inst))
		if it.workspaceDir != "" {
			branchByKey[runtimeKey(it.fleetName, it.instance)] = gitutil.BranchName(it.workspaceDir)
		}
	}
	// Bound the backend cache to the instances running this pass (stops/rebuilds
	// drop out). The stats pass owns this since it already enumerates every
	// running instance each tick.
	h.pruneBackends(activeKeys)

	// Re-install a missing Claude hook server-side (moved off the TUI's capture
	// poll). Fire-and-forget, dedup'd per container so a slow provision doesn't
	// stack across 3s ticks. Failures are surfaced as a per-instance warning
	// (the same ~/.fleet/logs/*.warn file the TUI reads).
	for _, it := range items {
		c, ok := captures[it.containerID]
		if !ok || !c.OK || !c.ClaudeHookMissing {
			continue
		}
		if _, busy := h.reprovisioning.LoadOrStore(it.containerID, struct{}{}); busy {
			continue
		}
		b := h.backendFor(it.inst)
		cid, wsDir, fleetName, instanceName := it.containerID, it.workspaceDir, it.fleetName, it.instance
		go func() {
			defer h.reprovisioning.Delete(cid)
			executor := agentdetect.NewBackendExecutor(b, wsDir)
			if err := agentdetect.NewClaudeProvisioner(executor).Provision(); err != nil {
				state.WriteWarn(fleetName, instanceName, fmt.Sprintf("claude hook reinstall failed: %v", err))
			}
		}()
	}

	now := time.Now()
	h.post(func(h *hub) {
		h.agent.Update(captures, probes, expectedIDs, now)
		ups := make([]runtimeUpdate, 0, len(items))
		for _, it := range items {
			cid := it.containerID
			key := runtimeKey(it.fleetName, it.instance)
			activity := agentActivityToProto(h.agent.State(cid))
			tool := agentToolToProto(h.agent.Tool(cid))
			stats := statsToProto(statsByID[cid])
			branch := branchByKey[key]
			ups = append(ups, runtimeUpdate{it.fleetName, it.instance, func(r *fleetgrpc.InstanceRuntime) {
				r.AgentActivity = activity
				r.AgentTool = tool
				r.Stats = stats
				if branch != "" {
					b := branch
					r.CurrentBranch = &b
				}
			}})
		}
		h.applyRuntime(ups)
	})
}

// sessionsPoller fills the live tmux session list for every running instance
// every ~1s. (The server has no UI "expanded" notion, so it polls all running
// instances; ExecCommandQuiet keeps it out of the event log.)
func sessionsPoller(ctx context.Context, h *hub) {
	ticker := time.NewTicker(sessionsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.runtimeWanted.Load() {
				sessionsPass(h)
			}
		}
	}
}

func sessionsPass(h *hub) {
	st, err := state.Load()
	if err != nil {
		return
	}
	type result struct {
		fleetName, instance string
		sessions            []*fleetgrpc.TmuxSession
	}
	var results []result
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.ContainerID == "" || inst.Status != fleet.StatusRunning {
				continue
			}
			// ListSessions execs directly against the container ID (no
			// devcontainer Node cold start) and returns "" on failure — which,
			// like a tmux server that isn't running, parses to an empty list and
			// clears any stale sessions.
			b := h.backendFor(inst)
			sessions := parseTmuxSessionsProto(b.ListSessions(inst.ContainerID))
			results = append(results, result{fleetName, inst.Name, sessions})
		}
	}
	if len(results) == 0 {
		return
	}
	h.post(func(h *hub) {
		ups := make([]runtimeUpdate, 0, len(results))
		for _, r := range results {
			sessions := r.sessions
			ups = append(ups, runtimeUpdate{r.fleetName, r.instance, func(rt *fleetgrpc.InstanceRuntime) {
				rt.Sessions = sessions
			}})
		}
		h.applyRuntime(ups)
	})
}
