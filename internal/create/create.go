package create

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Run performs instance creation for an instance that already exists
// in state.json with StatusCreating. For devcontainer backends it runs
// git clone + devcontainer up. For coder backends it runs coder create.
// On success it updates the instance to StatusRunning. On failure it sets
// StatusFailed with the error message.
//
// When branch is non-empty, the devcontainer clone uses `git clone
// --branch <branch>` so the instance is provisioned against that ref
// rather than the repository's default branch.
func Run(fleetName, instanceName, remoteURL, branch string, verbose bool, backendType fleet.BackendType) (err error) {
	start := time.Now()
	flog.Info("instance create started", "fleet", fleetName, "instance", instanceName, "backend", backendType, "branch", branch, "remote", remoteURL)
	// Log the failure outcome (with elapsed time) from one place: every error
	// return below flows through the named err, and setFailed only annotates
	// state. The success outcome is logged inline at the end where the
	// container ID is in scope.
	defer func() {
		if err != nil {
			flog.Error("instance create failed", "fleet", fleetName, "instance", instanceName, "backend", backendType, "ms", flog.MillisSince(start), "err", err)
		}
	}()
	if err := fleet.ValidateBackendType(backendType); err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	wsDir := filepath.Join(state.WorkspacesDir(), fleetName, instanceName, fleetName)

	var instanceBackend backend.Backend
	switch backendType {
	case fleet.BackendCoder:
		instanceBackend = buildCoderBackend(fleetName, instanceName, remoteURL, branch, verbose)
	case fleet.BackendCodespaces:
		instanceBackend = buildCodespacesBackend(remoteURL, branch, verbose)
	default:
		instanceBackend = backendutil.New(backendType, verbose)
	}

	if backendType != fleet.BackendCoder && backendType != fleet.BackendCodespaces {
		// Devcontainer: clone repo first, then provision
		if err := os.MkdirAll(filepath.Dir(wsDir), 0755); err != nil {
			setFailed(fleetName, instanceName, err)
			return fmt.Errorf("mkdir: %w", err)
		}

		cloneArgs := []string{"clone"}
		if branch != "" {
			cloneArgs = append(cloneArgs, "--branch", branch)
		}
		cloneArgs = append(cloneArgs, remoteURL, wsDir)
		gitClone := exec.Command("git", cloneArgs...)
		// Tee output to os.Stdout/os.Stderr (the log file when run from
		// the TUI) while capturing it for inclusion in error messages.
		var cloneBuf bytes.Buffer
		gitClone.Stdout = io.MultiWriter(os.Stdout, &cloneBuf)
		gitClone.Stderr = io.MultiWriter(os.Stderr, &cloneBuf)
		if err := gitClone.Run(); err != nil {
			wrapped := fmt.Errorf("git clone failed: %w\n%s", err, cloneBuf.String())
			setFailed(fleetName, instanceName, wrapped)
			return wrapped
		}
	}

	resolvedMounts, err := resolveProvisionMounts(instanceBackend, fleetName, instanceName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	result, err := instanceBackend.Up(wsDir, resolvedMounts.Mounts)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	// Run the post-Up provisioning (fleet-launch staging, dotfiles, startup
	// scripts, Claude hooks, shared caches). Shared with RunRebuild so a
	// rebuilt container ends up equivalently provisioned. Every step is
	// best-effort — the instance is marked running even if one fails.
	finishProvision(instanceBackend, fleetName, instanceName, wsDir, result.ContainerID, resolvedMounts.Symlinks)

	// Success: update state (instance is running even if dotfiles failed)
	if err := markInstanceRunning(fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance created", "fleet", fleetName, "instance", instanceName, "backend", backendType, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}
