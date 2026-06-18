package create

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// provision.go holds the provisioning steps shared by Run (create) and
// RunRebuild so those two paths stay in lock-step: resolveProvisionMounts
// assembles the mounts handed to the backend's Up/Rebuild, and finishProvision
// runs everything that has to happen once the container exists. RunClone does
// NOT use finishProvision — a clone inherits its source's dotfiles/startup
// effects via the committed image and deliberately must not re-run them — though
// it mirrors the same mount + re-stage steps inline (see clone.go).

// resolveProvisionMounts assembles the bind mounts for a provisioning run: the
// fleet's custom mounts, the per-instance control directory (so the host TUI
// can drive the in-instance `fleet launch` over a unix socket), and the shared
// buildkit socket (when the fleet enables it). The custom-mount resolution can
// hard-fail (returned error); the control + buildkit mounts are best-effort —
// a failure becomes a warning and the run continues, matching the surrounding
// pattern. Only devcontainer-style backends honor mounts; cloud-managed
// backends ignore them, so the host-side prep is skipped for those.
func resolveProvisionMounts(instanceBackend backend.Backend, fleetName, instanceName string) (mountresolver.Resolved, error) {
	resolvedMounts, err := resolveCustomMounts(instanceBackend, fleetName)
	if err != nil {
		return resolvedMounts, err
	}

	// Bind-mount the per-instance control directory so the host fleet TUI
	// can create a unix socket the in-instance `fleet launch` connects to.
	// A failure to create the host directory is non-fatal — the instance is
	// still usable, it just won't have in-instance control until recreated.
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

	return resolvedMounts, nil
}

// finishProvision runs the post-Up provisioning steps shared by Run and
// RunRebuild: stage the fleet-launch binary + fleet.rc, materialise mount
// symlinks, point docker buildx at the shared buildkit server, wire the
// instance into the fleet's shared deb/image caches, install dotfiles, run the
// per-fleet agent startup scripts, and install the Claude state-detection
// hooks. Every step is best-effort — a failure is surfaced as a warning and
// the instance is still marked running — so the order matters but no single
// step aborts the run. symlinks comes from the resolved mounts (the single-file
// agent-state links). containerID is the freshly-provisioned container's id,
// needed by the network-based cache wiring.
func finishProvision(instanceBackend backend.Backend, fleetName, instanceName, wsDir, containerID string, symlinks []mountresolver.Symlink) {
	// Stage the fleet-launch binary into the container so its in-container
	// subcommands (landing-page server, etc.) are ready without waiting for the
	// first browser open. Non-fatal: the browser-open path stages on demand too.
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, wsDir, nil); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("stage fleet-launch: %v", err))
	}

	// Stage fleet.rc into the container user's home so interactive shells can
	// source fleet-aware aliases. Respects the fleet's HomeDir setting; empty
	// falls back to fleetlaunch.DefaultHomeDir. Non-fatal.
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, wsDir, homeDirForFleet(fleetName), instanceName); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("stage fleet.rc: %v", err))
	}

	// Materialise the post-Up symlinks for any single-file mounts. Failure is
	// non-fatal — the instance is still usable; the agent will just have to log
	// in fresh.
	if err := applyMountSymlinks(instanceBackend, wsDir, symlinks); err != nil {
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
	// bind mount, so it runs here with containerID in scope), best-effort, and a
	// silent no-op for instances lacking apt / a local dockerd.
	if err := setupDebCache(instanceBackend, fleetName, containerID, wsDir); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("deb cache: %v", err))
	}
	if err := setupImageCache(instanceBackend, fleetName, instanceName, containerID, wsDir); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
	}

	// Auto-install dotfiles. A failure here is non-fatal — the instance is still
	// usable, so we surface the error as a warning rather than blocking.
	config, _ := state.LoadConfig()
	if config != nil && config.DotfilesSettings.AutoInstall {
		if script := dotfiles.SetupScript(config); script != "" {
			cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", script})
			out, err := cmd.CombinedOutput()
			if err != nil {
				detail := strings.TrimSpace(string(out))
				state.WriteWarn(fleetName, instanceName, fmt.Sprintf("dotfiles install failed: %v\n%s", err, detail))
			}
		}
	}

	// Run any agent install scripts associated with the fleet's settings (Claude
	// Code, Codex). Each script self-redirects its output to
	// ~/.fleet/startup/<name>.log inside the container, and failures are
	// non-fatal — the instance is still usable.
	runStartupScripts(instanceBackend, wsDir, fleetName, instanceName)

	// Install the agent state-detection hooks (Claude Code, auggie). Runs after
	// the startup scripts so any per-fleet agent install above has had a chance
	// to land before we drop our hooks alongside it. Non-fatal: the per-instance
	// Detector falls back to a safe default. We always install (regardless of
	// which agent the user actually runs) because the cost of unused hooks is
	// negligible.
	executor := agentdetect.NewBackendExecutor(instanceBackend, wsDir)
	if err := agentdetect.NewClaudeProvisioner(executor).Provision(); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("claude hook install failed: %v", err))
	}
	if err := agentdetect.NewAuggieProvisioner(executor).Provision(); err != nil {
		state.WriteWarn(fleetName, instanceName, fmt.Sprintf("auggie hook install failed: %v", err))
	}
}
