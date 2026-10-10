package server

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// branches.go resolves the git branch each instance's workspace has checked
// out. It is the only place a branch is read: the TUI row gets it from
// InstanceRuntime.current_branch (branchPoller), and `fleet ls` and the MCP
// fleet_list tool from GetState(fresh_branches) (freshBranches). Reads go
// through the backend's WorkspaceReader on the daemon, never a client, because
// a client may be on another machine than the checkout (the gateway, Armada
// SSH), and a backend may keep the workspace inside the instance.
//
// Every read that names a branch different from the instance's LastBranch is
// persisted, so a caller that gets no live read later (a stopped instance whose
// workspace lives only in its runtime, or a daemon that just restarted) still
// has the branch to show (fleet.DisplayBranch).

const (
	// branchPollInterval is how often the TUI's branches refresh, the cadence
	// they had while they rode along with the stats/activity pass.
	branchPollInterval = statsActivityInterval
	// branchGatePoll is how often the poller checks the runtime-wanted gate, so
	// a TUI that just subscribed gets its branches within about a second.
	branchGatePoll = time.Second
	// branchCacheTTL is how old a read GetState(fresh_branches) may reuse
	// instead of reading again, so a burst of `fleet ls` calls (or an MCP agent
	// polling fleet_list) costs one trip into each instance. Only cacheable
	// reads are reused (see branchRead).
	branchCacheTTL = 10 * time.Second
	// branchReadTimeout bounds one workspace read, so a wedged instance can't
	// stall `fleet ls` or the poller.
	branchReadTimeout = 5 * time.Second
	// branchReadConcurrency caps the workspace reads in flight at once.
	branchReadConcurrency = 8
)

// branchTarget is one instance whose branch is wanted.
type branchTarget struct {
	fleetName string
	inst      *fleet.Instance
}

func (t branchTarget) key() string { return runtimeKey(t.fleetName, t.inst.Name) }

// identity is what a cached read is only valid for: a rebuilt, restarted or
// recreated instance is read again.
func (t branchTarget) identity() string {
	return t.inst.WorkspaceDir + "\x00" + t.inst.ContainerID + "\x00" + string(t.inst.Status)
}

// branchRead is the result of one workspace read. ok is false when no live
// read was possible; see backend.WorkspaceReader.Branch. cacheable marks a
// read that went into the instance (a backend's own WorkspaceReader), which
// GetState may reuse for branchCacheTTL; a host checkout read costs a local
// git call and is always repeated, so `fleet ls` shows a switch at once.
type branchRead struct {
	branch    string
	ok        bool
	cacheable bool
}

type branchCacheEntry struct {
	read     branchRead
	identity string
	at       time.Time
}

// branchResolver reads instance branches with bounded concurrency and keeps
// the latest read per instance, shared by the poller (which always reads) and
// GetState (which reuses a read younger than branchCacheTTL).
type branchResolver struct {
	read func(ctx context.Context, inst *fleet.Instance) branchRead
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]branchCacheEntry
}

func newBranchResolver(read func(context.Context, *fleet.Instance) branchRead) *branchResolver {
	return &branchResolver{read: read, now: time.Now, entries: make(map[string]branchCacheEntry)}
}

// branchTargets lists every instance in st that has a workspace to read,
// sorted by fleet then instance.
func branchTargets(st *state.State) []branchTarget {
	var targets []branchTarget
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.WorkspaceDir == "" {
				continue
			}
			targets = append(targets, branchTarget{fleetName, inst})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].fleetName != targets[j].fleetName {
			return targets[i].fleetName < targets[j].fleetName
		}
		return targets[i].inst.Name < targets[j].inst.Name
	})
	return targets
}

// resolve returns each target's branch read, keyed by runtimeKey. A cacheable
// read younger than maxAge is reused (maxAge 0 reads every target). Callers pass
// every instance (branchTargets): entries for instances not among targets are
// dropped, which keeps the cache bounded to the instances that exist.
func (r *branchResolver) resolve(ctx context.Context, targets []branchTarget, maxAge time.Duration) map[string]branchRead {
	reads := make(map[string]branchRead, len(targets))
	var stale []branchTarget
	r.mu.Lock()
	now := r.now()
	wanted := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		wanted[t.key()] = struct{}{}
		e, ok := r.entries[t.key()]
		if ok && e.read.cacheable && maxAge > 0 && e.identity == t.identity() && now.Sub(e.at) < maxAge {
			reads[t.key()] = e.read
			continue
		}
		stale = append(stale, t)
	}
	for k := range r.entries {
		if _, ok := wanted[k]; !ok {
			delete(r.entries, k)
		}
	}
	r.mu.Unlock()

	results := make([]branchRead, len(stale))
	sem := make(chan struct{}, branchReadConcurrency)
	var wg sync.WaitGroup
	for i, t := range stale {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, inst *fleet.Instance) {
			defer wg.Done()
			defer func() { <-sem }()
			readCtx, cancel := context.WithTimeout(ctx, branchReadTimeout)
			defer cancel()
			rd := r.read(readCtx, inst)
			if readCtx.Err() != nil {
				// Timed out or the caller went away: not an answer to reuse.
				rd.cacheable = false
			}
			results[i] = rd
		}(i, t.inst)
	}
	wg.Wait()

	r.mu.Lock()
	at := r.now()
	for i, t := range stale {
		reads[t.key()] = results[i]
		r.entries[t.key()] = branchCacheEntry{read: results[i], identity: t.identity(), at: at}
	}
	r.mu.Unlock()
	return reads
}

// recordLastBranches persists each freshly read named branch that differs from
// the instance's LastBranch. A detached HEAD or a failed read keeps the last
// named branch.
func recordLastBranches(targets []branchTarget, reads map[string]branchRead) {
	changed := make(map[string]branchTarget)
	branches := make(map[string]string)
	for _, t := range targets {
		rd := reads[t.key()]
		if !rd.ok || rd.branch == "" || rd.branch == t.inst.LastBranch {
			continue
		}
		changed[t.key()] = t
		branches[t.key()] = rd.branch
	}
	if len(changed) == 0 {
		return
	}
	err := state.Update(func(st *state.State) error {
		for fleetName, f := range st.Fleets {
			for _, inst := range f.Instances {
				key := runtimeKey(fleetName, inst.Name)
				// Skip a record that was replaced since the read (same name, new
				// workspace): the read belongs to the old one.
				if t, ok := changed[key]; ok && inst.WorkspaceDir == t.inst.WorkspaceDir {
					inst.LastBranch = branches[key]
				}
			}
		}
		return nil
	})
	if err != nil {
		flog.Warn("record last branch failed", "instances", len(changed), "err", err)
	}
}

// readBranch is the hub's branch reader: the instance backend's WorkspaceReader
// (backendutil.WorkspaceReaderFor), so a backend that keeps its workspace in
// the instance reads it there and every other backend reads the host checkout.
func (h *hub) readBranch(ctx context.Context, inst *fleet.Instance) branchRead {
	reader := backendutil.WorkspaceReaderFor(h.workspaceBackend(inst))
	branch, ok := reader.Branch(ctx, inst.WorkspaceDir)
	_, onHost := reader.(backendutil.HostWorkspaceReader)
	return branchRead{branch: branch, ok: ok, cacheable: !onHost}
}

// workspaceBackend returns the backend to read inst's workspace through: the
// pollers' cached one while it runs (reused across ticks like every other
// poller read), a fresh one otherwise (the cache holds running instances only;
// see pruneBackends).
func (h *hub) workspaceBackend(inst *fleet.Instance) backend.Backend {
	if inst.ContainerID != "" && inst.Status == fleet.StatusRunning {
		return h.backendFor(inst)
	}
	return backendutil.NewForInstance(inst, false)
}

// branchPoller keeps InstanceRuntime.current_branch fresh for watching TUIs,
// for every instance with a workspace (stopped ones included: their reader may
// still answer, as the host checkout does). Gated on a runtime subscriber like
// the other runtime pollers; it also runs a pass as soon as the gate opens.
// The edge is seen on the next gate tick rather than via h.runtimeEdge, which
// is single-consumer (owned by liveStatusPoller), as prStatusPoller does.
func branchPoller(ctx context.Context, h *hub) {
	ticker := time.NewTicker(branchGatePoll)
	defer ticker.Stop()
	var lastRun time.Time
	wasWanted := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !h.runtimeWanted.Load() {
				wasWanted = false
				continue
			}
			edge := !wasWanted
			wasWanted = true
			if edge || time.Since(lastRun) >= branchPollInterval {
				branchPass(ctx, h)
				lastRun = time.Now()
			}
		}
	}
}

func branchPass(ctx context.Context, h *hub) {
	st, err := state.Load()
	if err != nil {
		return
	}
	targets := branchTargets(st)
	if len(targets) == 0 {
		return
	}
	reads := h.branches.resolve(ctx, targets, 0)
	recordLastBranches(targets, reads)
	ups := make([]runtimeUpdate, 0, len(targets))
	for _, t := range targets {
		rd := reads[t.key()]
		ups = append(ups, runtimeUpdate{t.fleetName, t.inst.Name, func(r *fleetgrpc.InstanceRuntime) {
			r.CurrentBranch = currentBranchField(rd)
		}})
	}
	h.post(func(h *hub) { h.applyRuntime(ups) })
}

// freshBranches answers GetState(fresh_branches): a runtime entry per instance
// with a workspace, carrying only current_branch (absent when no live read was
// possible). It reads through the same resolver as the poller, reusing reads
// younger than branchCacheTTL, so it works with no TUI watching.
func (h *hub) freshBranches(ctx context.Context, st *state.State) []*fleetgrpc.InstanceRuntime {
	targets := branchTargets(st)
	reads := h.branches.resolve(ctx, targets, branchCacheTTL)
	recordLastBranches(targets, reads)
	out := make([]*fleetgrpc.InstanceRuntime, 0, len(targets))
	for _, t := range targets {
		out = append(out, &fleetgrpc.InstanceRuntime{
			Fleet:         t.fleetName,
			Instance:      t.inst.Name,
			CurrentBranch: currentBranchField(reads[t.key()]),
		})
	}
	return out
}

// currentBranchField maps a read onto InstanceRuntime.current_branch: present
// (possibly "") for a live read, absent for none.
func currentBranchField(rd branchRead) *string {
	if !rd.ok {
		return nil
	}
	b := rd.branch
	return &b
}
