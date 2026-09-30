package resolver

import "github.com/BenjaminBenetti/fleet-man/internal/fleet"

// fileMountSpec describes a single-file mount expressed as a shared
// parent-directory bind mount plus a symlink. The file lives at the
// fleet's shared files directory on the host (one per filename) and
// is exposed inside the container via a symlink at symlinkTarget.
type fileMountSpec struct {
	// name is a human-readable identifier used in error messages.
	name string
	// filename is the basename of the file inside the shared mount.
	filename string
	// symlinkTarget is a container path, absolute or relative to the remote
	// workspace, where the symlink is created.
	symlinkTarget string
	// seedContent is written into the host file after the symlink is
	// established when the file is still empty (i.e. nothing to
	// migrate from the container). Empty means no seeding.
	seedContent string
}

// fileMountSpecsFor returns the fileMountSpecs implied by the enabled
// fields of fleetSettings. Single-file mounts ride on top of one
// shared parent-directory bind mount (sharedFilesContainerPath) and
// are surfaced inside the container as symlinks.
func fileMountSpecsFor(fleetSettings fleet.FleetSettings, containerHome string) []fileMountSpec {
	var specs []fileMountSpec
	if fleetSettings.ClaudeCodeMount {
		specs = append(specs, fileMountSpec{
			name:          "Claude Code config",
			filename:      ".claude.json",
			symlinkTarget: containerHome + "/.claude.json",
			// Claude Code crashes on install when ~/.claude.json
			// parses as anything other than valid JSON; an empty
			// file would trip that check.
			seedContent: "{}",
		}, fileMountSpec{
			name:     "Claude Code project settings",
			filename: "claude-settings.local.json",
			// Exec runs in the configured remote workspace, which need not be
			// /workspaces/<repo> (workspaceFolder can override it).
			symlinkTarget: ".claude/settings.local.json",
			seedContent:   "{}",
		})
	}
	return specs
}
