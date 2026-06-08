package create

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
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

	resolvedMounts, err := resolveCustomMounts(instanceBackend, fleetName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	// Bind-mount the per-instance control directory so the host fleet TUI
	// can create a unix socket the in-instance `fleet launch` connects to.
	// Only devcontainer-style backends honor custom mounts; cloud-managed
	// backends ignore them, so we skip the host-side prep for those. A
	// failure to create the host directory is non-fatal — the instance is
	// still usable, it just won't have in-instance control until recreated —
	// so we surface it as a warning and continue, matching the surrounding
	// best-effort pattern.
	if instanceBackend.SupportsCustomMounts() {
		mount, err := controlMount(fleetName, instanceName)
		if err != nil {
			state.WriteWarn(fleetName, instanceName, fmt.Sprintf("control mount: %v", err))
		} else {
			resolvedMounts.Mounts = append(resolvedMounts.Mounts, mount)
		}
	}

	// When the fleet enables a shared buildkit server, ensure its container is
	// running and bind its socket directory into the instance so docker buildx
	// can use the fleet-wide build cache. Non-fatal: a failure to start the
	// server just means this instance builds without the shared cache.
	if mount, ok, err := prepareBuildkitMount(instanceBackend, fleetName); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("buildkit server: %v", err))
	} else if ok {
		resolvedMounts.Mounts = append(resolvedMounts.Mounts, mount)
	}

	result, err := instanceBackend.Up(wsDir, resolvedMounts.Mounts)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	// Stage the fleet-launch binary into the container so its
	// in-container subcommands (landing-page server, etc.) are ready
	// without waiting for the first browser open. Non-fatal: the
	// browser-open path stages on demand too, so a failure here just
	// defers the work, it doesn't break the instance.
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, wsDir, nil); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("stage fleet-launch: %v", err))
	}

	// Stage fleet.rc into the container user's home so interactive
	// shells can source fleet-aware aliases. Respects the fleet's
	// HomeDir setting; empty falls back to fleetlaunch.DefaultHomeDir.
	// Non-fatal for the same reason as the binary stage.
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, wsDir, homeDirForFleet(fleetName)); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("stage fleet.rc: %v", err))
	}

	// Materialise the post-Up symlinks for any single-file mounts.
	// Failure is non-fatal — the instance is still usable; the agent
	// will just have to log in fresh — so we surface the error as a
	// warning and keep going.
	if err := applyMountSymlinks(instanceBackend, wsDir, resolvedMounts.Symlinks); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("setting up agentic mount symlinks: %v", err))
	}

	// Point the instance's docker buildx at the fleet's shared buildkit server
	// (when enabled). A silent no-op for images without docker/buildx, and
	// non-fatal otherwise — the instance is usable regardless of buildx wiring.
	if err := configureBuildkit(instanceBackend, fleetName, wsDir); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("configure buildkit buildx: %v", err))
	}

	// Wire the instance into the fleet's shared deb/image caches (when enabled):
	// ensure each cache server, join the instance to the fleet's shared docker
	// network, and write the in-instance apt/docker config. Network-based (no
	// bind mount, so it runs here with result.ContainerID in scope), best-effort,
	// and a silent no-op for instances lacking apt / a local dockerd.
	if err := setupDebCache(instanceBackend, fleetName, result.ContainerID, wsDir); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("deb cache: %v", err))
	}
	if err := setupImageCache(instanceBackend, fleetName, instanceName, result.ContainerID, wsDir); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
	}

	// Auto-install dotfiles. A failure here is non-fatal — the instance
	// is still usable, so we mark it running and surface the error as a
	// warning rather than blocking the whole creation.
	config, _ := state.LoadConfig()
	if config != nil && config.DotfilesSettings.AutoInstall {
		if script := dotfiles.SetupScript(config); script != "" {
			cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", script})
			out, err := cmd.CombinedOutput()
			if err != nil {
				detail := strings.TrimSpace(string(out))
				// Write warning to a file the TUI can pick up.
				state.WriteWarn(fleetName, instanceName, fmt.Sprintf("dotfiles install failed: %v\n%s", err, detail))
			}
		}
	}

	// Run any agent install scripts associated with the fleet's
	// settings (Claude Code, Codex). Each script self-redirects its
	// output to ~/.fleet/startup/<name>.log inside the container, and
	// failures are non-fatal — the instance is still usable, so we
	// surface them as a warning rather than aborting creation.
	runStartupScripts(instanceBackend, wsDir, fleetName, instanceName)

	// Install Claude Code state-detection hooks. Runs after the
	// startup scripts so any per-fleet Claude Code install above
	// has had a chance to land before we drop our hooks alongside
	// it. Like dotfiles, this is non-fatal: if hook installation
	// fails the user can still use the instance — fleet-man's
	// per-instance Detector falls back to a safe default for
	// Claude state. We always install (regardless of which agent
	// the user actually runs) because we don't know the choice at
	// creation time, and the cost of unused hooks is negligible.
	executor := agentdetect.NewBackendExecutor(instanceBackend, wsDir)
	if err := agentdetect.NewClaudeProvisioner(executor).Provision(); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("claude hook install failed: %v", err))
	}

	// Success: update state (instance is running even if dotfiles failed)
	if err := markInstanceRunning(fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance created", "fleet", fleetName, "instance", instanceName, "backend", backendType, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}
