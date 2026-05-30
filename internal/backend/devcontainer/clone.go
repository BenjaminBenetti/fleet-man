package devcontainer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// cloneImageLabel marks the docker image produced by `docker commit`
// during Clone. Down uses this label to identify and remove the
// committed image so cloned-instance teardown does not leak images.
const cloneImageLabel = "fleet.clone.image"

// devcontainerLocalFolderLabel and devcontainerConfigFileLabel are the
// two labels the devcontainer CLI uses to locate an existing container
// for a given workspace. Both must match the destination paths on the
// cloned container, otherwise `devcontainer exec` reports "Cannot find
// Dev Container" even though the container is running.
const (
	devcontainerLocalFolderLabel = "devcontainer.local_folder"
	devcontainerConfigFileLabel  = "devcontainer.config_file"
)

// inspectedContainer holds just the fields Clone needs from
// `docker inspect`. Decoded from the JSON array docker emits.
type inspectedContainer struct {
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Privileged  bool     `json:"Privileged"`
		CapAdd      []string `json:"CapAdd"`
		SecurityOpt []string `json:"SecurityOpt"`
		NetworkMode string   `json:"NetworkMode"`
		ExtraHosts  []string `json:"ExtraHosts"`
		Devices     []struct {
			PathOnHost        string `json:"PathOnHost"`
			PathInContainer   string `json:"PathInContainer"`
			CgroupPermissions string `json:"CgroupPermissions"`
		} `json:"Devices"`
	} `json:"HostConfig"`
}

// SupportsClone reports that the devcontainer backend can duplicate
// an instance via `docker commit` + `docker run`.
func (devcontainerBackend *DevcontainerBackend) SupportsClone() bool {
	return true
}

// Clone commits the source container into a private image, then runs a
// fresh container from that image with destWorkspaceDir bind-mounted at
// the same target path the source used. The committed image carries
// label fleet.clone.image=<tag> so Down can remove it on teardown.
//
// Source container state is irrelevant — `docker commit` works on
// running, paused, or stopped containers. The caller is expected to
// have already copied the source's workspace tree onto destWorkspaceDir
// before calling Clone, since bind mounts overlay the image layer.
func (devcontainerBackend *DevcontainerBackend) Clone(sourceContainerID, destWorkspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	if sourceContainerID == "" {
		return nil, fmt.Errorf("clone: source container ID is empty")
	}
	if destWorkspaceDir == "" {
		return nil, fmt.Errorf("clone: destination workspace dir is empty")
	}

	// Drop any docker container labelled with this dest workspace folder
	// before provisioning, mirroring Up's pruneStaleContainers contract
	// so a re-cloned name never silently binds to a leftover container.
	devcontainerBackend.pruneStaleContainers(destWorkspaceDir)

	inspected, err := inspectContainer(sourceContainerID)
	if err != nil {
		return nil, fmt.Errorf("clone: inspect source: %w", err)
	}

	workspaceTarget := findWorkspaceMountTarget(inspected)
	if workspaceTarget == "" {
		return nil, fmt.Errorf("clone: could not locate workspace mount target on source container %s", sourceContainerID)
	}

	imageTag, err := newCloneImageTag()
	if err != nil {
		return nil, fmt.Errorf("clone: generate image tag: %w", err)
	}

	commitCmd := exec.Command("docker", "commit", sourceContainerID, imageTag)
	commitCmd.Stdout = os.Stdout
	commitCmd.Stderr = os.Stderr
	if err := commitCmd.Run(); err != nil {
		return nil, fmt.Errorf("clone: docker commit failed: %w", err)
	}

	runArgs := buildCloneRunArgs(imageTag, destWorkspaceDir, workspaceTarget, inspected, mounts)
	runCmd := exec.Command("docker", runArgs...)
	out, err := runCmd.Output()
	if err != nil {
		// Best-effort cleanup of the committed image; ignore its error.
		_ = exec.Command("docker", "rmi", imageTag).Run()
		detail := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		if detail != "" {
			return nil, fmt.Errorf("clone: docker run failed: %w\n%s", err, detail)
		}
		return nil, fmt.Errorf("clone: docker run failed: %w", err)
	}

	newContainerID := strings.TrimSpace(string(out))
	if newContainerID == "" {
		_ = exec.Command("docker", "rmi", imageTag).Run()
		return nil, fmt.Errorf("clone: docker run returned empty container ID")
	}

	return &backend.UpResult{
		Outcome:               "success",
		ContainerID:           newContainerID,
		RemoteWorkspaceFolder: workspaceTarget,
	}, nil
}

// inspectContainer decodes the relevant slice of `docker inspect <id>`.
func inspectContainer(containerID string) (*inspectedContainer, error) {
	cmd := exec.Command("docker", "inspect", containerID)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	var inspected []inspectedContainer
	if err := json.Unmarshal(out, &inspected); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}
	if len(inspected) == 0 {
		return nil, fmt.Errorf("docker inspect returned no entries")
	}
	return &inspected[0], nil
}

// rebaseConfigFileLabel computes the destination's
// devcontainer.config_file label value by taking the source's
// config_file path (an absolute host path under the source's workspace
// folder) and rewriting it to live under destWorkspaceDir. Preserves
// the relative path of the config inside the workspace tree (typically
// .devcontainer/devcontainer.json, but the source may have used a
// custom location).
//
// Returns an empty string when the source has no config_file label or
// when the source's config path is not nested under the source's
// local_folder — both signal "skip the override" and let devcontainer
// CLI match by local_folder alone.
func rebaseConfigFileLabel(inspected *inspectedContainer, destWorkspaceDir string) string {
	if inspected.Config.Labels == nil {
		return ""
	}
	sourceConfigFile := inspected.Config.Labels[devcontainerConfigFileLabel]
	sourceLocalFolder := inspected.Config.Labels[devcontainerLocalFolderLabel]
	if sourceConfigFile == "" || sourceLocalFolder == "" {
		return ""
	}
	relPath, err := filepath.Rel(sourceLocalFolder, sourceConfigFile)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return ""
	}
	return filepath.Join(destWorkspaceDir, relPath)
}

// findWorkspaceMountTarget returns the container-side target path of the
// devcontainer workspace mount. The devcontainer CLI bind-mounts the
// workspace folder at /workspaces/<project>; we identify it by looking
// for a bind mount targeting a /workspaces/* path. Falls back to any
// bind mount under /workspaces/ if more than one is present.
func findWorkspaceMountTarget(inspected *inspectedContainer) string {
	for _, mount := range inspected.Mounts {
		if mount.Type != "bind" {
			continue
		}
		if strings.HasPrefix(mount.Destination, "/workspaces/") {
			return mount.Destination
		}
	}
	return ""
}

// buildCloneRunArgs assembles the `docker run` argv that starts a
// container from the committed image. Inherits CapAdd / SecurityOpt /
// Devices / Privileged / NetworkMode from the source's HostConfig so
// dev environments that needed extra privileges (docker-in-docker,
// ptrace, etc.) still work in the clone. Port bindings are
// deliberately not propagated — keeping them would collide with the
// source container if both are running.
func buildCloneRunArgs(imageTag, destWorkspaceDir, workspaceTarget string, inspected *inspectedContainer, mounts []backend.Mount) []string {
	args := []string{"run", "-d",
		"--label", devcontainerLocalFolderLabel + "=" + destWorkspaceDir,
		"--label", cloneImageLabel + "=" + imageTag,
		"--mount", "type=bind,source=" + destWorkspaceDir + ",target=" + workspaceTarget,
	}

	// Rebase the devcontainer.config_file label onto destWorkspaceDir.
	// The devcontainer CLI filters existing containers by BOTH
	// local_folder and config_file labels; without this override the
	// new container inherits the source's config_file path (committed
	// into the image) and `devcontainer exec` reports "Cannot find Dev
	// Container" even though the container is running. If the source
	// has no config_file label (unusual), skip — devcontainer CLI will
	// still match on local_folder alone in that case.
	if rebased := rebaseConfigFileLabel(inspected, destWorkspaceDir); rebased != "" {
		args = append(args, "--label", devcontainerConfigFileLabel+"="+rebased)
	}

	if inspected.HostConfig.Privileged {
		args = append(args, "--privileged")
	}
	for _, capability := range inspected.HostConfig.CapAdd {
		args = append(args, "--cap-add", capability)
	}
	for _, securityOpt := range inspected.HostConfig.SecurityOpt {
		args = append(args, "--security-opt", securityOpt)
	}
	for _, device := range inspected.HostConfig.Devices {
		spec := device.PathOnHost
		if device.PathInContainer != "" {
			spec += ":" + device.PathInContainer
		}
		if device.CgroupPermissions != "" {
			spec += ":" + device.CgroupPermissions
		}
		args = append(args, "--device", spec)
	}
	for _, host := range inspected.HostConfig.ExtraHosts {
		args = append(args, "--add-host", host)
	}
	if mode := inspected.HostConfig.NetworkMode; mode != "" && mode != "default" && mode != "bridge" {
		args = append(args, "--network", mode)
	}

	if sock := hostSSHAuthSock(); sock != "" {
		args = append(args, "--mount", "type=bind,source="+sock+",target="+containerSSHSocketPath)
	}

	args = append(args, customMountArgs(mounts)...)
	args = append(args, imageTag)
	return args
}

// newCloneImageTag returns a unique image tag of the form
// fleet-clone-<hex>:latest.
func newCloneImageTag() (string, error) {
	var randomBytes [6]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	return "fleet-clone-" + hex.EncodeToString(randomBytes[:]) + ":latest", nil
}

// cloneImageForContainer returns the fleet.clone.image label value for a
// container, or an empty string if the container does not carry the
// label (i.e. it was not produced by Clone). Used by Down to clean up
// committed images.
func cloneImageForContainer(containerID string) string {
	cmd := exec.Command("docker", "inspect", "--format",
		"{{ index .Config.Labels \""+cloneImageLabel+"\" }}", containerID)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
