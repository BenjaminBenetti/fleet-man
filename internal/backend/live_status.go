package backend

// LiveStatus represents a backend container/workspace's live state
// as reported by the underlying provider (docker, coder, gh).
//
// It is a coarser view than fleet.InstanceStatus: it only reflects
// what the backend itself can observe. The TUI maps LiveStatus into
// fleet.InstanceStatus, taking care to preserve transient states
// like creating/starting/stopping/deleting/failed that the backend
// has no concept of.
type LiveStatus string

const (
	// LiveStatusRunning means the container/workspace is up.
	LiveStatusRunning LiveStatus = "running"

	// LiveStatusStopped means the container/workspace exists but is halted.
	// This includes provider-driven stops (codespace idle timeout, coder
	// admin stop) and crashes — anything where the resource still exists
	// but is not currently executing.
	LiveStatusStopped LiveStatus = "stopped"

	// LiveStatusMissing means the container/workspace no longer exists
	// at the provider (deleted out-of-band).
	LiveStatusMissing LiveStatus = "missing"

	// LiveStatusUnknown means the probe failed for transient reasons —
	// network error, auth error, daemon not reachable. Callers must
	// treat this as inconclusive and preserve any persisted state.
	LiveStatusUnknown LiveStatus = "unknown"
)
