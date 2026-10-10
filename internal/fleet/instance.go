package fleet

import "time"

// Instance represents a single devcontainer workspace in a fleet.
//
// Name is the stable identifier used for container names, workspace paths,
// tmux session prefixes, and CLI lookups — it never changes after creation.
// DisplayName is the user-facing label shown in the TUI; editing it never
// touches underlying resources, but like Name it must satisfy
// ValidateInstanceName.
type Instance struct {
	Name         string         `json:"name"`
	DisplayName  string         `json:"display_name,omitempty"`
	ContainerID  string         `json:"container_id"`
	Config       string         `json:"config"`
	WorkspaceDir string         `json:"workspace_dir"`
	CreatedAt    time.Time      `json:"created_at"`
	Status       InstanceStatus `json:"status"`
	Error        string         `json:"error,omitempty"`
	Backend      BackendType    `json:"backend,omitempty"`
	Tag          string         `json:"tag,omitempty"`
	Color        string         `json:"color,omitempty"`
	Branch       string         `json:"branch,omitempty"`
	// LastBranch is the last named branch a live workspace read saw checked
	// out (see DisplayBranch). The server writes it when a read reports a
	// different branch and just before Stop, so callers that get no live read
	// — a stopped instance whose workspace lives only in its runtime, or a
	// daemon that just restarted — still have a branch to show.
	LastBranch string `json:"last_branch,omitempty"`
	// Automated marks an instance the automation scheduler spawned for a trigger
	// (issue #188), as opposed to one a user created. Set once at creation and
	// never cleared; the TUI shows a marker in front of its name.
	Automated bool `json:"automated,omitempty"`
	// KubeCluster is the cluster connection (KubeCluster.ID) a Kubernetes
	// instance runs on; empty for every other backend.
	KubeCluster string `json:"kube_cluster,omitempty"`
	// KubeKey is the instance's key on its cluster: it labels every object
	// the instance owns, and it is recorded on the CREATING/CLONING record
	// before any of them exists, so a failed or interrupted create can still
	// be torn down by key. Empty for every other backend.
	KubeKey string `json:"kube_key,omitempty"`
}

// GetDisplayName returns the user-facing label for the instance. Legacy
// instances persisted before DisplayName existed fall back to Name.
func (i *Instance) GetDisplayName() string {
	if i.DisplayName == "" {
		return i.Name
	}
	return i.DisplayName
}
