package fleet

// InstanceStatus represents the lifecycle state of an instance.
type InstanceStatus string

const (
	StatusCreating InstanceStatus = "creating"
	StatusCloning  InstanceStatus = "cloning"
	StatusRunning  InstanceStatus = "running"
	StatusStopped  InstanceStatus = "stopped"
	StatusFailed   InstanceStatus = "failed"
	StatusStopping InstanceStatus = "stopping"
	StatusStarting InstanceStatus = "starting"
	StatusDeleting InstanceStatus = "deleting"

	// StatusRebuilding marks an instance whose container is being torn down and
	// reprovisioned in place (the workspace is preserved). Transitional: the
	// rebuild job flips it back to StatusRunning (or StatusFailed) on completion.
	StatusRebuilding InstanceStatus = "rebuilding"
)
