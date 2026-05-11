package resolver

import "github.com/BenjaminBenetti/fleet-man/internal/fleet"

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
	return specs
}
