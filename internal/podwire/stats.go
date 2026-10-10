package podwire

// MethodStats is the agent RPC that reports the dev container's usage.
const MethodStats = "stats"

// StatsInfo is the "stats" RPC's result: the dev container's CPU and memory
// usage, by docker stats' formulas over the container's cgroup (CPU from the
// growth of its usage counter over wall time, memory as usage less the
// inactive file cache).
type StatsInfo struct {
	CPUMillicores float64 `json:"cpu_millicores"`
	MemoryBytes   int64   `json:"memory_bytes"`
}
