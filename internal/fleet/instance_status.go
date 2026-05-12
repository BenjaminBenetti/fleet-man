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
)
