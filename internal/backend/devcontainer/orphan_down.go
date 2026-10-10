package devcontainer

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// BindWorkspace binds the backend to the instance whose host workspace folder
// is workspaceDir, the key its containers carry (the devcontainer.local_folder
// label). backendutil.NewForInstance calls it.
func (devcontainerBackend *DevcontainerBackend) BindWorkspace(workspaceDir string) {
	devcontainerBackend.workspaceDir = workspaceDir
}

// BoundWorkspace is the workspace folder BindWorkspace bound ("" for none).
func (devcontainerBackend *DevcontainerBackend) BoundWorkspace() string {
	return devcontainerBackend.workspaceDir
}

// DownByKey removes, as Down does (volumes and clone image included), every
// container labelled with the bound workspace folder. That is what a create
// that failed after `devcontainer up` made its container leaves behind: a
// failing postCreateCommand ends the up with the container running and no
// ContainerID recorded, so Down never reaches it. It returns nil when nothing
// is bound, nothing is left, or there is no docker to have made anything.
func (devcontainerBackend *DevcontainerBackend) DownByKey() error {
	if devcontainerBackend.workspaceDir == "" {
		return nil
	}
	ids, err := workspaceContainers(devcontainerBackend.workspaceDir)
	if errors.Is(err, exec.ErrNotFound) {
		return nil // no docker on this host: no container can be left
	}
	if err != nil {
		return fmt.Errorf("find the containers of %s: %w", devcontainerBackend.workspaceDir, err)
	}
	var errs []error
	for _, id := range ids {
		if err := devcontainerBackend.Down(id); err != nil {
			errs = append(errs, fmt.Errorf("remove container %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// workspaceContainers lists the IDs of the containers, running or not,
// labelled with workspaceDir. A package var so tests can stand in for docker.
var workspaceContainers = dockerWorkspaceContainers

func dockerWorkspaceContainers(workspaceDir string) ([]string, error) {
	out, err := exec.Command("docker", "ps", "-a", "-q", "--no-trunc",
		"--filter", "label="+devcontainerLocalFolderLabel+"="+workspaceDir).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
