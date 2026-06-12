package resolver

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// customMountHostSubdir is the directory name (under a fleet's mount root on
// the host) beneath which every user-defined custom mount lives. A custom
// mount for container path "/opt/data" maps to host subdir ".mnt/opt/data".
const customMountHostSubdir = ".mnt"

// dirMountSpec describes a host↔container directory mapping that a
// fleet setting can request.
type dirMountSpec struct {
	// name is a human-readable identifier used in error messages.
	name string
	// hostSubdir is the relative path under the fleet's mount root.
	hostSubdir string
	// containerPath is the absolute path inside the container where
	// the host directory should appear.
	containerPath string
}

// dirMountSpecsFor returns the dirMountSpecs implied by the enabled
// fields of fleetSettings, anchored at the given containerHome.
// Adding a new directory mount toggle is one new entry here plus one
// new bool on fleet.FleetSettings.
func dirMountSpecsFor(fleetSettings fleet.FleetSettings, containerHome string) []dirMountSpec {
	var specs []dirMountSpec
	if fleetSettings.ClaudeCodeMount {
		specs = append(specs, dirMountSpec{
			name:          "Claude Code",
			hostSubdir:    ".claude",
			containerPath: containerHome + "/.claude",
		})
	}
	if fleetSettings.CodexMount {
		specs = append(specs, dirMountSpec{
			name:          "Codex",
			hostSubdir:    ".codex",
			containerPath: containerHome + "/.codex",
		})
	}
	if fleetSettings.GhMount {
		specs = append(specs, dirMountSpec{
			name:          "GitHub CLI",
			hostSubdir:    ".config/gh",
			containerPath: containerHome + "/.config/gh",
		})
	}
	if fleetSettings.AuggieMount {
		specs = append(specs, dirMountSpec{
			name:          "Auggie",
			hostSubdir:    ".augment",
			containerPath: containerHome + "/.augment",
		})
	}

	// User-defined custom mounts come AFTER the managed ones so that, when a
	// custom mount's container path collides with a managed target, the custom
	// mount is appended last and wins (the resolver/backend apply mounts in
	// order). Each entry is re-normalized defensively: persist-time validation
	// is authoritative, but a hand-edited state.json must never be allowed to
	// turn a ".." into a host path that escapes the fleet's .mnt directory, so
	// invalid entries are skipped rather than trusted.
	for _, raw := range fleetSettings.CustomMounts {
		containerPath, err := fleet.NormalizeCustomMount(raw)
		if err != nil {
			continue
		}
		specs = append(specs, dirMountSpec{
			name:          "custom mount " + containerPath,
			hostSubdir:    filepath.Join(customMountHostSubdir, strings.TrimPrefix(containerPath, "/")),
			containerPath: containerPath,
		})
	}

	// Collapse duplicate container paths keeping the LAST spec. Two specs
	// targeting the same container path would otherwise become two bind mounts
	// with the same destination, which the devcontainer/Docker backend rejects
	// outright ("Duplicate mount point"), failing the whole `fleet up`. Because
	// custom mounts are appended after the managed ones, keeping the last spec
	// makes a colliding custom mount win — the documented last-wins behavior —
	// instead of crashing provisioning.
	return dedupeSpecsByContainerPathKeepLast(specs)
}

// dedupeSpecsByContainerPathKeepLast returns specs with at most one entry per
// containerPath, retaining the LAST occurrence of each and preserving the
// relative order of the kept entries.
func dedupeSpecsByContainerPathKeepLast(specs []dirMountSpec) []dirMountSpec {
	seen := make(map[string]struct{}, len(specs))
	kept := make([]dirMountSpec, 0, len(specs))
	// Walk backwards so the first time we see a container path is its last
	// occurrence; reverse at the end to restore forward order.
	for i := len(specs) - 1; i >= 0; i-- {
		if _, ok := seen[specs[i].containerPath]; ok {
			continue
		}
		seen[specs[i].containerPath] = struct{}{}
		kept = append(kept, specs[i])
	}
	slices.Reverse(kept)
	return kept
}
