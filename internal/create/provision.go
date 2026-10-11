package create

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

// provision.go holds the provisioning steps shared by Run (create), RunRebuild
// and RunClone so those paths stay in lock-step: resolveProvisionMounts
// prepares the fleet's runtime and assembles the mounts handed to the
// backend's Up/Rebuild/Clone, and
// finishProvision runs everything that has to happen once the container exists.
// RunClone does NOT use finishProvision — a clone inherits its source's
// dotfiles/startup effects via the committed image and deliberately must not
// re-run them — though it mirrors the re-stage steps inline (see clone.go).
//
// services is the instance's shared-service runtime (svcruntime.ForInstance via
// sharedServicesFor), nil for an instance that gets no shared services.

// sharedServicesFor returns the runtime a provisioning job wires an instance's
// shared services (buildkit, deb cache, image cache) through, for the record
// the job binds its backend to. A package var so tests can substitute a
// recorder.
var sharedServicesFor = svcruntime.ForInstance

// resolveProvisionMounts prepares the fleet on the instance's shared-service
// runtime and assembles the mounts for a provisioning run: the
// fleet's shared and custom mounts, the per-instance control directory (so the
// host TUI can drive the in-instance `fleet launch` over a unix socket), and
// what the shared buildkit server needs (when the fleet enables it). The
// custom-mount resolution can hard-fail (returned error); the control + buildkit
// mounts are best-effort — a failure becomes a warning and the run continues,
// matching the surrounding pattern. Only backends that honor mounts get them;
// the host-side prep is skipped for the rest, and for a backend that carries
// the control channel itself there is no host control directory to mount.
// Warnings also go to ctx's provisioning log.
func resolveProvisionMounts(ctx context.Context, instanceBackend backend.Backend, services svcruntime.Runtime, fleetName, instanceName string) (mountresolver.Resolved, error) {
	resolvedMounts, err := resolveCustomMounts(instanceBackend, fleetName)
	if err != nil {
		return resolvedMounts, err
	}

	// What the fleet's instances on this runtime need before the first of
	// them is made there, whichever services are on (on a cluster: the
	// fleet's sibling DNS and its NetworkPolicies, which block the cloud
	// metadata range). Fatal: an instance that cannot get them would run
	// without its fleet's isolation.
	if services != nil {
		if err := services.PrepareFleet(fleetName); err != nil {
			return resolvedMounts, err
		}
	}

	// Bind-mount the per-instance control directory so the host fleet TUI
	// can create a unix socket the in-instance `fleet launch` connects to.
	// A failure to create the host directory is non-fatal — the instance is
	// still usable, it just won't have in-instance control until recreated.
	if mountsHostControlDir(instanceBackend) {
		mount, err := controlMount(fleetName, instanceName)
		if err != nil {
			provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("control mount: %v", err))
		} else {
			resolvedMounts.Mounts = append(resolvedMounts.Mounts, mount)
		}
	}

	// When the fleet enables a shared buildkit server, ensure it is running
	// and give the instance what it reaches it through (on docker, its socket
	// directory) so docker buildx can use the fleet-wide build cache.
	// Non-fatal: a failure to start the server just means this instance builds
	// without the shared cache.
	if mounts, err := prepareBuildkitMounts(services, fleetName); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("buildkit server: %v", err))
	} else {
		resolvedMounts.Mounts = append(resolvedMounts.Mounts, mounts...)
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
// needed by the network-based cache wiring. Each step is reported to ctx's
// provisioning log, and so is each warning.
func finishProvision(ctx context.Context, instanceBackend backend.Backend, services svcruntime.Runtime, fleetName, instanceName, wsDir, containerID string, symlinks []mountresolver.Symlink) {
	// Stage the fleet-launch binary into the container so its in-container
	// subcommands (landing-page server, etc.) are ready without waiting for the
	// first browser open. Non-fatal: the browser-open path stages on demand too.
	backend.ReportStep(ctx, fleet.StepStageFleetLaunch, "")
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, wsDir, nil); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("stage fleet-launch: %v", err))
	}

	// Stage fleet.rc into the container user's home so interactive shells can
	// source fleet-aware aliases. Respects the fleet's HomeDir setting; empty
	// falls back to fleetlaunch.DefaultHomeDir. Non-fatal.
	backend.ReportStep(ctx, fleet.StepStageFleetRC, "")
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, wsDir, homeDirForFleet(fleetName), instanceName); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("stage fleet.rc: %v", err))
	}

	// Materialise the post-Up symlinks for any single-file mounts. Failure is
	// non-fatal — the instance is still usable; the agent will just have to log
	// in fresh.
	if len(symlinks) > 0 {
		backend.ReportStep(ctx, fleet.StepApplyMountSymlinks, "")
	}
	if err := applyMountSymlinks(instanceBackend, wsDir, symlinks); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("setting up agentic mount symlinks: %v", err))
	}

	// Point the instance's docker buildx at the fleet's shared buildkit server
	// (when enabled). A silent no-op for images without docker/buildx, and
	// non-fatal otherwise — the instance is usable regardless of buildx wiring.
	backend.ReportStep(ctx, fleet.StepConfigureCaches, "")
	if err := configureBuildkit(services, instanceBackend, fleetName, wsDir); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("configure buildkit buildx: %v", err))
	}

	// Wire the instance into the fleet's shared deb/image caches (when enabled):
	// ensure each cache server, connect the instance to them (on docker, join
	// the fleet's shared network), and write the in-instance apt/docker config.
	// Network-based (no mount, so it runs here with containerID in scope),
	// best-effort, and a silent no-op for instances lacking apt / a local dockerd.
	if err := setupDebCache(services, instanceBackend, fleetName, containerID, wsDir); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("deb cache: %v", err))
	}
	if err := setupImageCache(services, instanceBackend, fleetName, instanceName, containerID, wsDir); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
	}

	// Auto-install dotfiles. Best-effort and bounded: a dotfiles install that
	// hangs (e.g. a git clone stuck on an unreachable host) must not stall the
	// whole instance creation, so installDotfiles kills each attempt after a
	// timeout and retries a few times before giving up and letting the instance
	// start anyway. A failure is surfaced as a warning rather than blocking.
	config, _ := state.LoadConfig()
	if config != nil && config.DotfilesSettings.AutoInstall {
		if script := dotfiles.SetupScript(config); script != "" {
			backend.ReportStep(ctx, fleet.StepInstallDotfiles, "")
			installDotfiles(ctx, instanceBackend, fleetName, instanceName, wsDir, script)
		}
	}

	// Run any agent install scripts associated with the fleet's settings (Claude
	// Code, Codex). Each script self-redirects its output to
	// ~/.fleet/startup/<name>.log inside the container, and failures are
	// non-fatal — the instance is still usable.
	backend.ReportStep(ctx, fleet.StepRunStartupScripts, "")
	runStartupScripts(ctx, instanceBackend, wsDir, fleetName, instanceName)

	// Install the agent state-detection hooks (Claude Code, auggie). Runs after
	// the startup scripts so any per-fleet agent install above has had a chance
	// to land before we drop our hooks alongside it. Non-fatal: the per-instance
	// Detector falls back to a safe default. We always install (regardless of
	// which agent the user actually runs) because the cost of unused hooks is
	// negligible.
	backend.ReportStep(ctx, fleet.StepInstallClaudeHooks, "")
	executor := agentdetect.NewBackendExecutor(instanceBackend, wsDir)
	if err := agentdetect.NewClaudeProvisioner(executor).Provision(); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("claude hook install failed: %v", err))
	}
	if err := agentdetect.NewAuggieProvisioner(executor).Provision(); err != nil {
		provisionWarning(ctx, fleetName, instanceName, fmt.Sprintf("auggie hook install failed: %v", err))
	}
}
