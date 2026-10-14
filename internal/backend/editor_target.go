package backend

import "context"

// EditorTarget is an optional capability for a backend whose instances VS
// Code can open over Remote-SSH, through the in-instance `fleet ssh-server`
// (sshserver): Kubernetes instances always, devcontainer instances when the
// editor is not on the daemon's host. The daemon reads what the editor
// applies on attach from it (the merged devcontainer metadata and the folder
// it opens), so ssh-server can apply the customizations the Dev Containers
// extension would. Backends whose editors attach another way (Coder,
// Codespaces) do not implement it.
type EditorTarget interface {
	// EditorInfo reads the instance's container (containerID, the record's).
	EditorInfo(ctx context.Context, containerID string) (EditorInfo, error)
}

// EditorInfo is what an editor attaching to an instance needs from it.
type EditorInfo struct {
	// WorkspaceFolder is the devcontainer's workspace folder inside the
	// instance.
	WorkspaceFolder string
	// Metadata is the container's devcontainer.metadata label: the JSON array
	// of merged metadata entries (devmeta.EditorFromMetadata reads it).
	Metadata string
}
