package backend

// UpResult holds the outcome of provisioning a workspace.
type UpResult struct {
	Outcome               string `json:"outcome"`
	ContainerID           string `json:"containerId"`
	RemoteUser            string `json:"remoteUser"`
	RemoteWorkspaceFolder string `json:"remoteWorkspaceFolder"`
	// Warnings are what the backend accepted short of what the instance
	// asked for (admission dropping a debug grant, an image that needs a
	// pull login), one line each, already in the job's log. The create puts
	// them on the instance's banner once its own best-effort steps, which
	// replace the banner, are done.
	Warnings []string `json:"-"`
}
