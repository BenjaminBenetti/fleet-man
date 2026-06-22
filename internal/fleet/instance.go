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
	// Automated marks an instance the automation scheduler spawned for a trigger
	// (issue #188), as opposed to one a user created. Set once at creation and
	// never cleared; the TUI shows a marker in front of its name.
	Automated bool `json:"automated,omitempty"`
}

// GetDisplayName returns the user-facing label for the instance. Legacy
// instances persisted before DisplayName existed fall back to Name.
func (i *Instance) GetDisplayName() string {
	if i.DisplayName == "" {
		return i.Name
	}
	return i.DisplayName
}
