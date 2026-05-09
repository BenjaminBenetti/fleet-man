package coder

// coderAgent represents a single agent in a coder workspace.
type coderAgent struct {
	Name              string  `json:"name"`
	Status            string  `json:"status"`
	LifecycleState    string  `json:"lifecycle_state"`
	ParentID          *string `json:"parent_id"` // non-nil for devcontainer agents
	Directory         string  `json:"directory"`
	ExpandedDirectory string  `json:"expanded_directory"`
}
