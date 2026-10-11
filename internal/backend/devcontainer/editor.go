package devcontainer

import (
	"context"
	"errors"
	"path"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

var _ backend.EditorTarget = (*DevcontainerBackend)(nil)

// inspectForEditor reads a container for EditorInfo (a var for tests).
var inspectForEditor = inspectContainer

// EditorInfo reads what VS Code applies on attach from the container the
// devcontainer CLI made (backend.EditorTarget): the devcontainer.metadata
// label the Dev Containers extension reads, and the folder the workspace is
// mounted at. Used when the editor attaches over Remote-SSH (a remote
// daemon's instance); a local attach opens the dev-container URI instead.
func (devcontainerBackend *DevcontainerBackend) EditorInfo(_ context.Context, containerID string) (backend.EditorInfo, error) {
	if containerID == "" {
		return backend.EditorInfo{}, errors.New("the instance has no container")
	}
	inspected, err := inspectForEditor(containerID)
	if err != nil {
		return backend.EditorInfo{}, err
	}
	return editorInfoOf(inspected), nil
}

// editorInfoOf is the EditorInfo of an inspected container: its metadata
// label, and the folder its local workspace (devcontainer.local_folder) is
// bind-mounted at, else /workspaces/<the workspace's base name> as the CLI
// defaults it.
func editorInfoOf(inspected *inspectedContainer) backend.EditorInfo {
	labels := inspected.Config.Labels
	info := backend.EditorInfo{Metadata: labels[devcontainerMetadataLabel]}
	local := labels[devcontainerLocalFolderLabel]
	for _, m := range inspected.Mounts {
		if local != "" && m.Type == "bind" && filepath.Clean(m.Source) == filepath.Clean(local) {
			info.WorkspaceFolder = m.Destination
			return info
		}
	}
	if local != "" {
		info.WorkspaceFolder = path.Join("/workspaces", filepath.Base(local))
	}
	return info
}
