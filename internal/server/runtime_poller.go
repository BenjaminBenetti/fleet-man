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
	liveStatusInterval     = time.Minute
	statsActivityInterval  = 3 * time.Second
	sessionsPollInterval   = time.Second
	tmuxListSessionsFormat = `tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`
)

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

// liveStatusPoller probes each instance's backend live status (running/stopped/
// missing/unknown) for the runtime sidecar ONLY — it never writes state.json
// (the TUI's live_status.go still owns the persisted running<->stopped flip in
// P2). Runs on the 60s tick and immediately on the false->true gate edge so a
// freshly-subscribed TUI gets a live_status hint without waiting a minute.
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
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.ContainerID == "" {
				continue
			}
			if inst.Status != fleet.StatusRunning && inst.Status != fleet.StatusStopped {
				continue
			}
			b := backendutil.NewForInstance(inst, false)
			ls := liveStatusToProto(b.Status(inst.ContainerID))
			ups = append(ups, runtimeUpdate{fleetName, inst.Name, func(r *fleetgrpc.InstanceRuntime) {
				r.LiveStatus = ls
			}})
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

	for _, it := range items {
		b := backendutil.NewForInstance(it.inst, false)
		if m, err := b.Stats([]string{it.containerID}); err == nil {
			if s := m[it.containerID]; s != nil {
				statsByID[it.containerID] = s
			}
		}
		captures[it.containerID] = b.CaptureAllSessions(it.containerID)
		if tool, ok := b.AgentToolProbe(it.containerID); ok {
			probes[it.containerID] = tool
		}
		expectedIDs = append(expectedIDs, it.containerID)
		if it.workspaceDir != "" {
			branchByKey[runtimeKey(it.fleetName, it.instance)] = gitutil.BranchName(it.workspaceDir)
		}
	}

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
		b := backendutil.NewForInstance(it.inst, false)
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
			b := backendutil.NewForInstance(inst, false)
			cmd := b.ExecCommandQuiet(inst.WorkspaceDir, []string{"sh", "-c", tmuxListSessionsFormat})
			out, err := cmd.Output()
			var sessions []*fleetgrpc.TmuxSession
			if err == nil {
				// tmux exits non-zero when no server is running; that simply
				// means "no sessions", which clears any stale list.
				sessions = parseTmuxSessionsProto(string(out))
			}
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
