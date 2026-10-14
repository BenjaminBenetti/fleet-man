package backend

import "time"

// RestartAware is an optional capability for backends whose runtime restarts a
// running instance on its own. Kubernetes restarts a pod's container after an
// OOM kill or a crash, and recreates the pod after an eviction, a node drain or
// a node loss, while fleet still has the instance running. Docker never does
// that to fleet's containers: one that dies is seen stopped, and the next
// Start re-runs what a start runs.
//
// A restart ends every tmux session and program in the instance and skips
// fleet's post-start work, so the daemon reads it here to re-run that work
// (postStart) and to tell the user on the instance row why their sessions
// ended ("sessions reset: <cause>").
type RestartAware interface {
	// LastRestart reports the latest restart of the instance the runtime made
	// on its own. ok is false when there has been none since fleet last
	// started the instance, or when it can't be told right now.
	LastRestart(containerID string) (restart Restart, ok bool)
}

// Restart is one restart the runtime made without fleet asking.
type Restart struct {
	// ID tells restarts apart: it is different for each one (Kubernetes: the
	// pod UID and the container's restart count), so a caller that remembers
	// the last ID it handled handles each restart once.
	ID string
	// Cause is why it happened.
	Cause RestartCause
	// At is when the instance came back.
	At time.Time
}

// RestartCause is why the runtime restarted an instance, in the words the
// instance row shows after "sessions reset: ".
type RestartCause string

const (
	RestartOOMKilled   RestartCause = "OOM-killed"
	RestartEvicted     RestartCause = "evicted"
	RestartNodeDrained RestartCause = "node drained"
	RestartNodeLost    RestartCause = "node lost"
	RestartFleetMoved  RestartCause = "fleet moved"
	RestartCrashed     RestartCause = "crashed"
	// RestartPodDeleted: someone deleted the instance's pod (kubectl delete
	// pod, an operator) with none of the cluster's own reasons on it.
	RestartPodDeleted RestartCause = "pod deleted"
)

// PostStarter is an optional capability of a RestartAware backend: it redoes
// the devcontainer's start-time hooks (postStartCommand, then
// postAttachCommand) in an instance that is running again after a restart the
// runtime made on its own, as its Start does after a Fleet start. The daemon
// calls it once per Restart.
type PostStarter interface {
	RunPostStart(containerID string) error
}
