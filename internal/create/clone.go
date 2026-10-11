package create

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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
//  1. Look up the source instance from state and build the backend,
//     bound to the destination record.
//  2. Refuse early if the backend does not support cloning so the
//     caller surfaces a clear error instead of partial work.
//  3. Copy the source workspace directory onto the destination
//     workspace dir. Bind mounts overlay the committed image layer, so
//     the workspace tree must exist on disk before the new container
//     starts. A backend that keeps the workspace itself
//     (backend.WorkspaceOwner: Kubernetes, on the instance's disk) copies
//     it with the rest of the instance instead, and the host copy is
//     skipped.
//  4. Hand off to the backend's clone (backend.CloneContext), which captures
//     the source's filesystem state and starts the new container.
//  5. Persist the new ContainerID and flip the instance to
//     StatusRunning.
//
// Unlike create.Run, RunClone does NOT re-run dotfiles / startup
// scripts: the source already executed those, and the committed image
// carries their effects. Running them again would diverge the clone
// from the source.
//
// ctx is the daemon job's: cancelling it (a destroy of the destination, or
// the daemon shutting down) stops the clone at the next step, tearing down
// a container it already started (see cancel.go).
func RunClone(ctx context.Context, fleetName, srcInstance, destInstance string, verbose bool) (err error) {
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

	destWorkspaceDir := filepath.Join(state.WorkspacesDir(), fleetName, destInstance, fleetName)

	// The clone acts through a backend bound to the DESTINATION record (which
	// startCloneInstanceJob created with the source's backend): what it creates
	// belongs to the destination, and the source is reached by its ContainerID.
	// A destination record removed while the job was starting falls back to the
	// fields the clone has always used.
	dest := persistedRecord(st, fleetName, destInstance)
	if dest == nil {
		dest = &fleet.Instance{Name: destInstance, WorkspaceDir: destWorkspaceDir, Status: fleet.StatusCloning, Backend: src.Backend}
	}
	instanceBackend := newInstanceBackend(dest, verbose)
	services := sharedServicesFor(dest)
	if !instanceBackend.SupportsClone() {
		err := fmt.Errorf("backend %q does not support cloning", dest.Backend)
		setFailed(fleetName, destInstance, err)
		return err
	}

	if err := os.MkdirAll(filepath.Dir(destWorkspaceDir), 0755); err != nil {
		err = fmt.Errorf("mkdir dest workspace parent: %w", err)
		setFailed(fleetName, destInstance, err)
		return err
	}

	// Copy the source workspace tree. `cp -a` preserves permissions,
	// timestamps, and symlinks — important for git's working tree
	// metadata and any tooling that inspects mtimes. A backend that owns its
	// workspaces copies the source's with the rest of the instance.
	if err := checkCancelled(ctx); err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}
	if !backend.OwnsWorkspace(instanceBackend) {
		backend.ReportStep(ctx, fleet.StepCopyWorkspace, "from "+srcInstance)
		if err := copyWorkspaceTree(ctx, src.WorkspaceDir, destWorkspaceDir); err != nil {
			err = cancelledOr(ctx, err)
			setFailed(fleetName, destInstance, err)
			return err
		}
	}

	// The clone gets the same mounts a create would, for the destination: the
	// fleet's shared and custom mounts, its own control directory (so the host
	// fleet TUI can drive the in-instance `fleet launch` browser control over
	// a unix socket) and the shared buildkit server's (ensuring the server is
	// up), so it shares the same build cache. Only the shared-mount resolution
	// can fail the clone; the rest only warns.
	backend.ReportStep(ctx, fleet.StepResolveMounts, "")
	resolvedMounts, err := resolveProvisionMounts(ctx, instanceBackend, services, fleetName, destInstance)
	if err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}

	if err := checkCancelled(ctx); err != nil {
		setFailed(fleetName, destInstance, err)
		return err
	}
	backend.ReportStep(ctx, fleet.StepBackendClone, "from "+srcInstance)
	// CloneContext hands the job's ctx (cancellation, JobLog, Hooks) to a
	// backend whose clone is a cancellable multi-step sequence, and falls back
	// to the plain Clone for the rest.
	result, err := backend.CloneContext(ctx, instanceBackend, src.ContainerID, backend.UpRequest{WorkspaceDir: destWorkspaceDir, Mounts: resolvedMounts.Mounts})
	if err != nil {
		if errors.Is(err, backend.ErrCheckpointed) {
			// The daemon is shutting down, and the backend left the clone at
			// a checkpoint (its journal) that the next start settles the
			// record from: the record stays as it is until then.
			return err
		}
		err = cancelledOr(ctx, err)
		setFailed(fleetName, destInstance, err)
		return err
	}
	// A plain Clone takes no context: stop a clone the run was cancelled
	// during.
	if err := stopIfCancelled(ctx, instanceBackend, fleetName, destInstance, result.ContainerID); err != nil {
		return err
	}

	// Re-stage the fleet-launch binary and rc in the clone. /usr/bin/fleet
	// and ~/.fleet/fleet.rc both ride along in the docker commit, so the clone
	// inherits whatever the source had — which may be stale if the host
	// has been updated since the source was staged. EnsureFresh skips
	// when the binary already matches; EnsureFleetRC always rewrites,
	// which is cheap (small file, one exec) and also re-stamps
	// FLEET_INSTANCE_NAME with the clone's own name rather than the
	// source's. Non-fatal like in Run.
	backend.ReportStep(ctx, fleet.StepStageFleetLaunch, "")
	if _, err := fleetlaunch.EnsureFresh(instanceBackend, destWorkspaceDir, nil); err != nil {
		provisionWarning(ctx, fleetName, destInstance, fmt.Sprintf("stage fleet-launch: %v", err))
	}
	backend.ReportStep(ctx, fleet.StepStageFleetRC, "")
	if err := fleetlaunch.EnsureFleetRC(instanceBackend, destWorkspaceDir, homeDirForFleet(fleetName), destInstance); err != nil {
		provisionWarning(ctx, fleetName, destInstance, fmt.Sprintf("stage fleet.rc: %v", err))
	}

	// Materialise post-clone symlinks the same way Up does so agent
	// state mounts (e.g. ~/.claude.json) are re-linked inside the clone.
	// Non-fatal: surface as a warning if it fails.
	if len(resolvedMounts.Symlinks) > 0 {
		backend.ReportStep(ctx, fleet.StepApplyMountSymlinks, "")
	}
	if err := applyMountSymlinks(instanceBackend, destWorkspaceDir, resolvedMounts.Symlinks); err != nil {
		provisionWarning(ctx, fleetName, destInstance, fmt.Sprintf("setting up agentic mount symlinks: %v", err))
	}

	// Re-point the clone's docker buildx at the fleet's shared buildkit server.
	// The committed image may carry the source's builder config; the
	// remove-then-create in ConfigureInstanceBuildx makes this idempotent.
	// Non-fatal, and a silent no-op without docker/buildx.
	backend.ReportStep(ctx, fleet.StepConfigureCaches, "")
	if err := configureBuildkit(services, instanceBackend, fleetName, destWorkspaceDir); err != nil {
		provisionWarning(ctx, fleetName, destInstance, fmt.Sprintf("configure buildkit buildx: %v", err))
	}

	// Wire the clone into the fleet's shared deb/image caches the same way Run
	// does (ensure server, connect the clone, write in-instance config).
	// The committed image may carry the source's apt/docker config; the writes
	// are idempotent. Best-effort, silent no-op without apt / a local dockerd.
	if err := setupDebCache(services, instanceBackend, fleetName, result.ContainerID, destWorkspaceDir); err != nil {
		provisionWarning(ctx, fleetName, destInstance, fmt.Sprintf("deb cache: %v", err))
	}
	if err := setupImageCache(services, instanceBackend, fleetName, destInstance, result.ContainerID, destWorkspaceDir); err != nil {
		provisionWarning(ctx, fleetName, destInstance, fmt.Sprintf("image cache: %v", err))
	}

	// The backend's own warnings (a clone copied live, best-effort) go in
	// front of what the steps above wrote over the banner.
	prependBackendWarnings(fleetName, destInstance, result.Warnings)

	backend.ReportStep(ctx, fleet.StepMarkRunning, "")
	if err := commitRunning(ctx, instanceBackend, fleetName, destInstance, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance cloned", "fleet", fleetName, "src", srcInstance, "instance", destInstance, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}

// copyWorkspaceTree runs `cp -a <src>/. <dest>/` so the contents of src
// land directly inside dest (mirroring rsync's trailing-slash idiom).
// The trailing `/.` is the portable way to ask cp to copy contents
// rather than the directory itself. Cancelling ctx kills the copy.
func copyWorkspaceTree(ctx context.Context, src, dest string) error {
	if src == "" {
		return fmt.Errorf("source workspace dir is empty")
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source workspace dir %s: %w", src, err)
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("mkdir dest workspace dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-a", src+"/.", dest+"/")
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(stepOutput(ctx), &buf)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cp -a %s -> %s: %w\n%s", src, dest, err, buf.String())
	}
	return nil
}
