package create

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// RunClone duplicates an existing instance into destInstance. The
// destination record must already exist in state.json with
// StatusCloning (the CLI and TUI both pre-create it that way so the
// operation is visible in the UI while the docker commit + run is in
// flight).
//
// The flow:
//
//  1. Look up the source instance from state and build its backend.
//  2. Refuse early if the backend does not support cloning so the
//     caller surfaces a clear error instead of partial work.
//  3. Copy the source workspace directory onto the destination
//     workspace dir. Bind mounts overlay the committed image layer, so
//     the workspace tree must exist on disk before the new container
//     starts.
//  4. Hand off to backend.Clone, which captures the source's filesystem
//     state and starts the new container.
//  5. Persist the new ContainerID and flip the instance to
//     StatusRunning.
//
// Unlike create.Run, RunClone does NOT re-run dotfiles / startup
// scripts: the source already executed those, and the committed image
// carries their effects. Running them again would diverge the clone
// from the source.
func RunClone(fleetName, srcInstance, destInstance string, verbose bool) (err error) {
	start := time.Now()
	flog.Info("instance clone started", "fleet", fleetName, "src", srcInstance, "instance", destInstance)
	// Failure outcome (with elapsed time) is logged from one place; setFailed
	// only annotates state. Success is logged inline at the end.
	defer func() {
		if err != nil {
			flog.Error("instance clone failed", "fleet", fleetName, "src", srcInstance, "instance", destInstance, "ms", flog.MillisSince(start), "err", err)
		}
	}()
	st, err := state.Load()
	if err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		err := fmt.Errorf("fleet %q not found", fleetName)
		setFailed(fleetName, destInstance, err)
		return err
	}
	src, err := f.GetInstance(srcInstance)
	if err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}
	if src.ContainerID == "" {
		err := fmt.Errorf("source instance %s/%s has no container id (status: %s)", fleetName, srcInstance, src.Status)
		setFailed(fleetName, destInstance, err)
		return err
	}

	instanceBackend := backendutil.NewForInstance(src, verbose)
	if !instanceBackend.SupportsClone() {
		err := fmt.Errorf("backend %q does not support cloning", src.Backend)
		setFailed(fleetName, destInstance, err)
		return err
	}

	destWorkspaceDir := filepath.Join(state.WorkspacesDir(), fleetName, destInstance, fleetName)
	if err := os.MkdirAll(filepath.Dir(destWorkspaceDir), 0755); err != nil {
		err = fmt.Errorf("mkdir dest workspace parent: %w", err)
		setFailed(fleetName, destInstance, err)
		return err
	}

	// Copy the source workspace tree. `cp -a` preserves permissions,
	// timestamps, and symlinks — important for git's working tree
	// metadata and any tooling that inspects mtimes.
	if err := copyWorkspaceTree(src.WorkspaceDir, destWorkspaceDir); err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}

	resolvedMounts, err := resolveCustomMounts(instanceBackend, fleetName)
	if err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}

	// Bind-mount the per-instance control directory into the clone, the same
	// way Run does, so the host fleet TUI can drive the in-instance
	// `fleet launch` browser control over a unix socket. Guard with
	// SupportsCustomMounts (only devcontainer-style backends honor mounts)
	// and treat a host-directory failure as non-fatal — surface a warning
	// and keep going rather than aborting the clone.
	if instanceBackend.SupportsCustomMounts() {
		mount, err := controlMount(fleetName, destInstance)
		if err != nil {
			state.WriteWarn(fleetName, destInstance, fmt.Sprintf("control mount: %v", err))
		} else {
			resolvedMounts.Mounts = append(resolvedMounts.Mounts, mount)
		}
	}

	result, err := instanceBackend.Clone(src.ContainerID, destWorkspaceDir, resolvedMounts.Mounts)
	if err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}

	// Re-stage the fleet-launch binary and rc in the clone. /usr/bin/fleet
	// and ~/.fleet/fleet.rc both ride along in the docker commit, so the clone
	// inherits whatever the source had — which may be stale if the host
	// has been updated since the source was staged. EnsureFresh skips
	// when the binary already matches; EnsureFleetRC always rewrites,
	// which is cheap (small file, one exec). Non-fatal like in Run.
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, destWorkspaceDir, nil); err != nil {
		state.WriteWarn(fleetName, destInstance, fmt.Sprintf("stage fleet-launch: %v", err))
	}
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, destWorkspaceDir, homeDirForFleet(fleetName)); err != nil {
		state.WriteWarn(fleetName, destInstance, fmt.Sprintf("stage fleet.rc: %v", err))
	}

	// Materialise post-clone symlinks the same way Up does so agent
	// state mounts (e.g. ~/.claude.json) are re-linked inside the clone.
	// Non-fatal: surface as a warning if it fails.
	if err := applyMountSymlinks(instanceBackend, destWorkspaceDir, resolvedMounts.Symlinks); err != nil {
		state.WriteWarn(fleetName, destInstance, fmt.Sprintf("setting up agentic mount symlinks: %v", err))
	}

	if err := markInstanceRunning(fleetName, destInstance, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance cloned", "fleet", fleetName, "src", srcInstance, "instance", destInstance, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}

// copyWorkspaceTree runs `cp -a <src>/. <dest>/` so the contents of src
// land directly inside dest (mirroring rsync's trailing-slash idiom).
// The trailing `/.` is the portable way to ask cp to copy contents
// rather than the directory itself.
func copyWorkspaceTree(src, dest string) error {
	if src == "" {
		return fmt.Errorf("source workspace dir is empty")
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source workspace dir %s: %w", src, err)
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("mkdir dest workspace dir: %w", err)
	}

	cmd := exec.Command("cp", "-a", src+"/.", dest+"/")
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cp -a %s -> %s: %w\n%s", src, dest, err, buf.String())
	}
	return nil
}
