// Package resolver translates fleet-level mount settings into concrete
// host→container bind mounts (and, where needed, symlinks layered on
// top of those mounts). It owns the mapping between agent-specific
// toggles (Claude Code, Codex, …) and the filesystem layout under
// ~/.fleet/workspaces/<fleet>/, so the rest of the codebase can treat
// mounts as a generic primitive.
package resolver

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
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
// inside the container after Up. Every mount it returns is
// MountScopeFleetShared, and every path it creates is recorded in
// Resolved.Layout.
//
// Home-level mounts are rooted at fleetSettings.HomeDir; project file links
// are relative to the remote workspace. When HomeDir is empty Resolve falls
// back to defaultContainerHome — picked to match standard Microsoft
// devcontainer images — so existing fleets created before the home-dir
// detector existed still get a working mount on the common case.
//
// Returns a zero-value Resolved when no mounts are enabled. An error
// is returned when a host directory or file cannot be created, or when
// a symlink sits at (or above) one of them under the fleet's mount root
// (a SymlinkError, refused and logged rather than followed; see
// hostLayout), leaving the caller to decide whether to abort
// provisioning or proceed without the mount.
func Resolve(fleetName string, fleetSettings fleet.FleetSettings) (Resolved, error) {
	containerHome := fleetSettings.HomeDir
	if containerHome == "" {
		containerHome = defaultContainerHome
	}

	dirSpecs := dirMountSpecsFor(fleetSettings, containerHome)
	fileSpecs := fileMountSpecsFor(fleetSettings, containerHome)
	if len(dirSpecs) == 0 && len(fileSpecs) == 0 {
		return Resolved{}, nil
	}

	var resolved Resolved
	fleetMountRoot := fleetMountDir(fleetName)
	layout, err := openHostLayout(fleetMountRoot)
	if err != nil {
		return Resolved{}, fmt.Errorf("preparing fleet mount root: %w", err)
	}
	defer layout.Close()

	// Directory mounts.
	for _, spec := range dirSpecs {
		if err := layout.ensureDir(spec.hostSubdir); err != nil {
			return Resolved{}, logRefusal(fleetName, spec.prepareError(err))
		}
		subPath := filepath.ToSlash(spec.hostSubdir)
		resolved.Layout = append(resolved.Layout, LayoutEntry{Path: subPath, Dir: true, Mode: layoutDirMode})
		resolved.Mounts = append(resolved.Mounts, backend.Mount{
			LocalPath:     filepath.Join(fleetMountRoot, spec.hostSubdir),
			ContainerPath: spec.containerPath,
			Scope:         backend.MountScopeFleetShared,
			SubPath:       subPath,
		})
	}

	// File mounts: one shared host directory bind-mounted into the
	// container, plus a symlink per file pointing the agent's
	// expected path at the file inside the shared mount.
	if len(fileSpecs) > 0 {
		if err := layout.ensureDir(sharedFilesSubdir); err != nil {
			return Resolved{}, logRefusal(fleetName, fmt.Errorf("preparing shared files mount: %w%s",
				err, symlinkFix(err, "replace it with a real folder")))
		}
		resolved.Layout = append(resolved.Layout, LayoutEntry{Path: sharedFilesSubdir, Dir: true, Mode: layoutDirMode})
		resolved.Mounts = append(resolved.Mounts, backend.Mount{
			LocalPath:     filepath.Join(fleetMountRoot, sharedFilesSubdir),
			ContainerPath: sharedFilesContainerPath,
			Scope:         backend.MountScopeFleetShared,
			SubPath:       sharedFilesSubdir,
		})
		for _, spec := range fileSpecs {
			if err := layout.ensureFile(filepath.Join(sharedFilesSubdir, spec.filename)); err != nil {
				return Resolved{}, logRefusal(fleetName, fmt.Errorf("preparing %s file: %w%s",
					spec.name, err, symlinkFix(err, "replace it with a regular file")))
			}
			resolved.Layout = append(resolved.Layout, LayoutEntry{Path: sharedFilesSubdir + "/" + spec.filename, Mode: layoutFileMode})
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

// logRefusal records a refused symlink in fleet.log and returns err unchanged.
// The instance's error carries the same message; the log keeps the path for
// later, after the record is gone.
func logRefusal(fleetName string, err error) error {
	var symlinkErr *SymlinkError
	if errors.As(err, &symlinkErr) {
		flog.Warn("mount refused: symlink in fleet mount layout", "fleet", fleetName, "path", symlinkErr.Path, "err", err)
	}
	return err
}
