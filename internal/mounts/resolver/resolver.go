// Package resolver translates fleet-level mount settings into concrete
// host→container bind mounts. It owns the mapping between agent-specific
// toggles (Claude Code, Codex, …) and the filesystem layout under
// ~/.fleet/workspaces/<fleet>/, so the rest of the codebase can treat
// mounts as a generic primitive.
package resolver

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// ===========================================
// Public API
// ===========================================

// Resolve translates the mount-related fields of fleetSettings into a
// slice of backend.Mount values for the named fleet. For each enabled
// setting it ensures the host-side directory exists (creating it with
// 0700 permissions if needed) so the bind mount target is valid before
// the backend's Up call runs.
//
// The container side of each mount is rooted at fleetSettings.HomeDir
// (the user's container home). When HomeDir is empty Resolve falls
// back to defaultContainerHome — picked to match standard Microsoft
// devcontainer images — so existing fleets created before the home-dir
// detector existed still get a working mount on the common case.
//
// Returns an empty slice when no mounts are enabled. An error is
// returned only when a host directory cannot be created, leaving the
// caller to decide whether to abort provisioning or proceed without
// the mount.
func Resolve(fleetName string, fleetSettings fleet.FleetSettings) ([]backend.Mount, error) {
	containerHome := fleetSettings.HomeDir
	if containerHome == "" {
		containerHome = defaultContainerHome
	}

	mountSpecs := mountSpecsFor(fleetSettings, containerHome)
	if len(mountSpecs) == 0 {
		return nil, nil
	}

	fleetMountRoot := fleetMountDir(fleetName)
	mounts := make([]backend.Mount, 0, len(mountSpecs))
	for _, spec := range mountSpecs {
		hostPath := filepath.Join(fleetMountRoot, spec.hostSubdir)
		if err := ensureHostDir(hostPath); err != nil {
			return nil, fmt.Errorf("preparing %s mount: %w", spec.name, err)
		}
		mounts = append(mounts, backend.Mount{
			LocalPath:     hostPath,
			ContainerPath: spec.containerPath,
		})
	}
	return mounts, nil
}

// ===========================================
// Internal types
// ===========================================

// mountSpec describes a single host↔container mapping that a fleet
// setting can request. The host path is computed by joining the fleet's
// mount root with hostSubdir; the container path is used verbatim.
type mountSpec struct {
	// name is a human-readable identifier used in error messages.
	name string
	// hostSubdir is the relative path under the fleet's mount root.
	hostSubdir string
	// containerPath is the absolute path inside the container where the
	// host directory should appear.
	containerPath string
}

// ===========================================
// Internal helpers
// ===========================================

// mountSpecsFor returns the mountSpecs implied by the enabled fields of
// fleetSettings, anchored at the given containerHome. Adding a new
// agentic mount toggle is one new entry in this function plus one new
// bool on fleet.FleetSettings.
func mountSpecsFor(fleetSettings fleet.FleetSettings, containerHome string) []mountSpec {
	var specs []mountSpec
	if fleetSettings.ClaudeCodeMount {
		specs = append(specs, mountSpec{
			name:          "Claude Code",
			hostSubdir:    ".claude",
			containerPath: containerHome + "/.claude",
		})
	}
	if fleetSettings.CodexMount {
		specs = append(specs, mountSpec{
			name:          "Codex",
			hostSubdir:    ".codex",
			containerPath: containerHome + "/.codex",
		})
	}
	return specs
}

// defaultContainerHome is used when FleetSettings.HomeDir is empty —
// either because the user has not run the home-dir detector yet, or
// because the fleet predates the field. Matches the user created by
// Microsoft's standard devcontainer base images.
const defaultContainerHome = "/home/vscode"

// fleetMountDir returns the host directory under which all of a fleet's
// shared mount targets live. Lives next to (not inside) the per-instance
// workspace clones so it survives instance churn.
func fleetMountDir(fleetName string) string {
	return filepath.Join(state.WorkspacesDir(), fleetName)
}

// ensureHostDir creates path with 0700 permissions if it does not yet
// exist. Permissions are restrictive because the directories typically
// hold authentication tokens (Claude/Codex login state).
func ensureHostDir(path string) error {
	return os.MkdirAll(path, 0700)
}
