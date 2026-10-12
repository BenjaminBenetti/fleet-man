package podwire

import "time"

// The agent RPCs a clone's consistent copy uses. A privileged pod has the
// user cgroup pod-init laid out (fleet-user: the start script and every
// command the agent starts for a user); freezing it stops every user process
// while the agent, PID 1 and the watchdog, all in fleet-agent, stay live.
const (
	// MethodFreeze freezes the user cgroup (FreezeArgs → FreezeInfo) and
	// flushes the instance disk once it is still. A pod without the layout
	// (unprivileged: a read-only cgroupfs) answers CodeUnsupported.
	MethodFreeze = "freeze"
	// MethodThaw thaws it again (→ ThawInfo); thawing a pod that is not
	// frozen is no error.
	MethodThaw = "thaw"
	// MethodSync flushes the instance disk (→ SyncInfo): what a snapshot of
	// a live pod's disk is taken after.
	MethodSync = "sync"
)

// CodeUnsupported is a request the pod cannot do (a freeze without a cgroup
// layout).
const CodeUnsupported = "unsupported"

// CodeThawed is a renewal of a freeze that is no longer in force: its
// watchdog (or a thaw) ended it, so what it held still has run on since.
const CodeThawed = "thawed"

// Bounds of a freeze. The watchdog thaws a freeze nobody thawed by its
// deadline: the agent's own timer, and pod-init, PID 1, which outlives the
// agent and fleetd and thaws a little later from the freeze's record.
const (
	DefaultFreezeWatchdog = 30 * time.Second
	MaxFreezeWatchdog     = 2 * time.Minute
)

// FreezeArgs are MethodFreeze's arguments.
type FreezeArgs struct {
	// WatchdogMS is how long the freeze may last, from its reply (once the
	// processes stopped and the disk flushed), before the pod thaws itself;
	// 0 is DefaultFreezeWatchdog, and it is capped at MaxFreezeWatchdog.
	WatchdogMS int64 `json:"watchdog_ms,omitempty"`
	// Renew moves the deadline of the freeze in force and never freezes
	// anew: a freeze its watchdog ended meanwhile answers CodeThawed, so the
	// holder learns the user processes ran on (a renewal that froze them
	// again would hide it).
	Renew bool `json:"renew,omitempty"`
}

// Watchdog is the freeze's bound, defaulted and capped.
func (a FreezeArgs) Watchdog() time.Duration {
	d := time.Duration(a.WatchdogMS) * time.Millisecond
	switch {
	case d <= 0:
		return DefaultFreezeWatchdog
	case d > MaxFreezeWatchdog:
		return MaxFreezeWatchdog
	}
	return d
}

// FreezeInfo is MethodFreeze's result.
type FreezeInfo struct {
	// FrozenAt is when every user process had stopped; Deadline when the
	// watchdog thaws them if no thaw comes first.
	FrozenAt time.Time `json:"frozen_at"`
	Deadline time.Time `json:"deadline"`
	// SettleMS is how long the processes took to stop; SyncMS how long the
	// disk took to flush after.
	SettleMS int64 `json:"settle_ms"`
	SyncMS   int64 `json:"sync_ms"`
}

// ThawInfo is MethodThaw's result.
type ThawInfo struct {
	// WasFrozen: the user cgroup was still frozen when the thaw came.
	WasFrozen bool `json:"was_frozen"`
	// ByWatchdog: the watchdog had thawed it already (the freeze outlasted
	// its deadline, so what was copied after that was copied live).
	ByWatchdog bool `json:"by_watchdog,omitempty"`
	// ThawedAt is when the cgroup thawed (this call, or the watchdog).
	ThawedAt time.Time `json:"thawed_at"`
}

// SyncInfo is MethodSync's result.
type SyncInfo struct {
	SyncMS int64 `json:"sync_ms"`
}
