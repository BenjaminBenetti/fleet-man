package create

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
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
//
// A file:// remote is a local TEMPLATE directory (fleet.IsTemplateRemote):
// instead of cloning, its contents are copied into the workspace verbatim.
// Only the devcontainer and kubernetes backends can do that (coder and
// codespaces provision from a git URL on their own side), and a branch has no
// meaning for a copy.
func Run(fleetName, instanceName, remoteURL, branch string, verbose bool, backendType fleet.BackendType) (err error) {
	return RunContext(context.Background(), fleetName, instanceName, remoteURL, branch, verbose, backendType)
}

// RunContext allows the daemon to ask a connected client about an unknown git
// host key before provisioning fails. The context belongs to the daemon job:
// cancelling it (a destroy of the instance, or the daemon shutting down) stops
// the run at the next step, tearing down a container it already started (see
// cancel.go).
func RunContext(ctx context.Context, fleetName, instanceName, remoteURL, branch string, verbose bool, backendType fleet.BackendType) (err error) {
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
	if err := fleet.ValidateTemplateCreate(remoteURL, branch, backendType); err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	wsDir := filepath.Join(state.WorkspacesDir(), fleetName, instanceName, fleetName)

	backend.ReportStep(ctx, fleet.StepBuildBackend, string(backendType))
	rec := createRecord(fleetName, instanceName, wsDir, branch, backendType)
	instanceBackend := createBackend(rec, fleetName, remoteURL, branch, verbose)
	services := sharedServicesFor(rec)

	if err := checkCancelled(ctx); err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	switch {
	case backendType == fleet.BackendCoder || backendType == fleet.BackendCodespaces:
		// Remote-provisioned backends clone on their own side.
	case fleet.IsTemplateRemote(remoteURL):
		// Template fleet: copy the local template dir into the workspace
		// (the cp stands in for git clone), then provision.
		backend.ReportStep(ctx, fleet.StepCopyWorkspace, "template "+remoteURL)
		if err := copyTemplateTree(ctx, remoteURL, wsDir); err != nil {
			err = cancelledOr(ctx, err)
			setFailed(fleetName, instanceName, err)
			return err
		}
	default:
		// Devcontainer: clone repo first, then provision
		if err := os.MkdirAll(filepath.Dir(wsDir), 0755); err != nil {
			setFailed(fleetName, instanceName, err)
			return fmt.Errorf("mkdir: %w", err)
		}

		if err := cloneWorkspace(ctx, remoteURL, branch, wsDir); err != nil {
			setFailed(fleetName, instanceName, err)
			return err
		}
	}

	backend.ReportStep(ctx, fleet.StepResolveMounts, "")
	resolvedMounts, err := resolveProvisionMounts(ctx, instanceBackend, services, fleetName, instanceName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	if err := checkCancelled(ctx); err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}
	backend.ReportStep(ctx, fleet.StepBackendUp, string(backendType))
	// UpContext hands the job's ctx (cancellation, JobLog, Hooks) to a backend
	// whose provisioning is a cancellable multi-step sequence, and falls back to
	// the plain Up for the rest.
	result, err := backend.UpContext(ctx, instanceBackend, backend.UpRequest{WorkspaceDir: wsDir, Mounts: resolvedMounts.Mounts})
	if err != nil {
		if errors.Is(err, backend.ErrCheckpointed) {
			// The daemon is shutting down, and the backend left the run at
			// a checkpoint (its journal) that the next start settles the
			// record from: the record stays as it is until then.
			return err
		}
		// A ctx-aware backend stopped by the cancellation reports why.
		err = cancelledOr(ctx, err)
		setFailed(fleetName, instanceName, err)
		return err
	}
	// A plain Up takes no context, so the run may have been cancelled while
	// it worked: stop here rather than provision the new container further.
	if err := stopIfCancelled(ctx, instanceBackend, fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}

	// Run the post-Up provisioning (fleet-launch staging, dotfiles, startup
	// scripts, Claude hooks, shared caches). Shared with RunRebuild so a
	// rebuilt container ends up equivalently provisioned. Every step is
	// best-effort — the instance is marked running even if one fails.
	finishProvision(ctx, instanceBackend, services, fleetName, instanceName, wsDir, result.ContainerID, resolvedMounts.Symlinks)

	// The backend's own warnings go in front of what the steps above wrote
	// over the banner (the template warning below still takes the first line).
	prependBackendWarnings(fleetName, instanceName, result.Warnings)

	// Template worktree/submodule check LAST, and prepended: every provisioning
	// step above WriteWarns (replacing the banner) on failure, and the TUI shows
	// the banner's first line — this warning must win that slot.
	if fleet.IsTemplateRemote(remoteURL) {
		if warn := templateGitFileWarning(wsDir); warn != "" {
			state.PrependWarn(fleetName, instanceName, warn)
			backend.ReportWarning(ctx, warn)
		}
	}

	// Success: update state (instance is running even if dotfiles failed),
	// unless the run was abandoned during the provisioning steps above.
	backend.ReportStep(ctx, fleet.StepMarkRunning, "")
	if err := commitRunning(ctx, instanceBackend, fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance created", "fleet", fleetName, "instance", instanceName, "backend", backendType, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}

// cloneWorkspace git clones remoteURL (at branch, when set) into wsDir,
// whose parent exists. A clone that fails says why, with a hint for the
// failures a fix is known for; a cancelled one reports the cancellation.
func cloneWorkspace(ctx context.Context, remoteURL, branch, wsDir string) error {
	cloneArgs := []string{"clone"}
	if branch != "" {
		cloneArgs = append(cloneArgs, "--branch", branch)
	}
	cloneArgs = append(cloneArgs, "--", remoteURL, wsDir)
	backend.ReportStep(ctx, fleet.StepGitClone, branchDetail(branch))
	out, err := gitutil.Clone(ctx, remoteURL, cloneArgs, stepOutput(ctx))
	if err != nil {
		// A cancelled clone was killed: report why, not "signal: killed".
		if cerr := checkCancelled(ctx); cerr != nil {
			return cerr
		}
		detail := string(out)
		if hint := gitutil.CloneFailureHint(detail, remoteURL, agentsock.CloneForwarding()); hint != "" {
			detail = strings.TrimRight(detail, "\n") + "\n" + hint + "\n"
		}
		return fmt.Errorf("git clone failed: %w\n%s", err, detail)
	}
	return nil
}

// branchDetail names the branch a clone checks out, for its step line.
func branchDetail(branch string) string {
	if branch == "" {
		return ""
	}
	return "branch " + branch
}
