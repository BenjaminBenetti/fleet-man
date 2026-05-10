// Package resolver translates fleet-level mount settings into concrete
// host→container bind mounts (and, where needed, symlinks layered on
// top of those mounts). It owns the mapping between agent-specific
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
// Constants
// ===========================================

// defaultContainerHome is used when FleetSettings.HomeDir is empty —
// either because the user has not run the home-dir detector yet, or
// because the fleet predates the field. Matches the user created by
// Microsoft's standard devcontainer base images.
const defaultContainerHome = "/home/vscode"

// sharedFilesSubdir is the directory name (under a fleet's mount
// root on the host) that holds every single-file mount. Living in
// one shared directory keeps the bind-mount count to one regardless
// of how many file mounts are enabled.
const sharedFilesSubdir = "files"

// sharedFilesContainerPath is the absolute path inside the container
// where the shared files directory is mounted. Lives at the root
// (rather than under the user's home) so it never collides with
// agent-managed paths and so its top-level dir creation does not
// require user-level permissions.
const sharedFilesContainerPath = "/fleet-mounts/files"

// ===========================================
// Public API
// ===========================================

// Resolve translates the mount-related fields of fleetSettings into a
// Resolved bundle for the named fleet. For each enabled directory
// setting it ensures the host-side directory exists; for each enabled
// single-file setting it ensures the host file exists and adds both a
// shared parent-dir mount and a Symlink that the caller must create
// inside the container after Up.
//
// The container side of each mount is rooted at fleetSettings.HomeDir
// (the user's container home). When HomeDir is empty Resolve falls
// back to defaultContainerHome — picked to match standard Microsoft
// devcontainer images — so existing fleets created before the home-dir
// detector existed still get a working mount on the common case.
//
// Returns a zero-value Resolved when no mounts are enabled. An error
// is returned only when a host directory or file cannot be created,
// leaving the caller to decide whether to abort provisioning or
// proceed without the mount.
func Resolve(fleetName string, fleetSettings fleet.FleetSettings) (Resolved, error) {
	containerHome := fleetSettings.HomeDir
	if containerHome == "" {
		containerHome = defaultContainerHome
	}

	var resolved Resolved
	fleetMountRoot := fleetMountDir(fleetName)

	// Directory mounts.
	for _, spec := range dirMountSpecsFor(fleetSettings, containerHome) {
		hostPath := filepath.Join(fleetMountRoot, spec.hostSubdir)
		if err := ensureHostDir(hostPath); err != nil {
			return Resolved{}, fmt.Errorf("preparing %s mount: %w", spec.name, err)
		}
		resolved.Mounts = append(resolved.Mounts, backend.Mount{
			LocalPath:     hostPath,
			ContainerPath: spec.containerPath,
		})
	}

	// File mounts: one shared host directory bind-mounted into the
	// container, plus a symlink per file pointing the agent's
	// expected path at the file inside the shared mount.
	fileSpecs := fileMountSpecsFor(fleetSettings, containerHome)
	if len(fileSpecs) > 0 {
		sharedHostDir := filepath.Join(fleetMountRoot, sharedFilesSubdir)
		if err := ensureHostDir(sharedHostDir); err != nil {
			return Resolved{}, fmt.Errorf("preparing shared files mount: %w", err)
		}
		resolved.Mounts = append(resolved.Mounts, backend.Mount{
			LocalPath:     sharedHostDir,
			ContainerPath: sharedFilesContainerPath,
		})
		for _, spec := range fileSpecs {
			hostFile := filepath.Join(sharedHostDir, spec.filename)
			if err := ensureHostFile(hostFile); err != nil {
				return Resolved{}, fmt.Errorf("preparing %s file: %w", spec.name, err)
			}
			resolved.Symlinks = append(resolved.Symlinks, Symlink{
				Source:      sharedFilesContainerPath + "/" + spec.filename,
				Target:      spec.symlinkTarget,
				SeedContent: spec.seedContent,
			})
		}
	}

	return resolved, nil
}

// ===========================================
// Internal helpers
// ===========================================

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

// ensureHostFile creates an empty file at path with 0600 permissions
// when one does not already exist, leaving any existing content
// untouched. The parent directory must already exist (callers run
// ensureHostDir on it first).
func ensureHostFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}
