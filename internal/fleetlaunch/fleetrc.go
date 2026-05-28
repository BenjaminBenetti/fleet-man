package fleetlaunch

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// DefaultHomeDir is the in-container home directory used when a fleet
// has no HomeDir setting persisted. Matches the user created by
// Microsoft's standard devcontainer base images and mirrors the same
// default used by the mount resolver, so an unconfigured fleet's rc
// lands in the same place its mounts use.
const DefaultHomeDir = "/home/vscode"

// rcSubdir is the home-relative directory that holds the in-container
// fleet rc. Namespaced under ~/.fleet/ to match every other in-container
// fleet path (~/.fleet/startup, ~/.fleet/workspaces, …).
const rcSubdir = ".fleet"

// rcFilename is the basename of the rc file written into rcSubdir.
const rcFilename = "fleet.rc"

// bashrcMarker is the fixed substring grep'd against ~/.bashrc to
// detect a prior wire-in of the source block. Specific enough to never
// collide; checking just for the path means a hand-edited dupe gets
// detected and skipped too.
const bashrcMarker = ".fleet/fleet.rc"

//go:embed fleet.rc
var fleetRCContent string

// EnsureFleetRC writes the embedded fleet.rc into the container at
// <homeDir>/.fleet/fleet.rc and ensures the user's ~/.bashrc sources
// it on shell startup. homeDir is the absolute path of the in-container
// home (from FleetSettings.HomeDir); an empty string falls back to
// DefaultHomeDir.
//
// Both steps are idempotent:
//   - the rc itself is overwritten on every call. The content ships
//     with the fleet binary, so the latest rc reaches the container
//     alongside every binary refresh; the payload is small enough that
//     skipping a diff check costs nothing.
//   - the .bashrc source block is appended only when the marker isn't
//     already present, so re-stages don't accumulate duplicates.
//
// All paths live in the user's home, so no sudo is needed.
func EnsureFleetRC(b backend.Backend, workspaceDir, homeDir string) error {
	if homeDir == "" {
		homeDir = DefaultHomeDir
	}
	rcDir := homeDir + "/" + rcSubdir
	target := rcDir + "/" + rcFilename
	bashrc := homeDir + "/.bashrc"

	// 1. Write the rc. mkdir -p is idempotent; cat streams the embedded
	// content over stdin so the body never has to be shell-quoted into
	// the script literal.
	write := fmt.Sprintf(`mkdir -p %s && cat > %s`, rcDir, target)
	cmd := b.ExecCommand(workspaceDir, []string{"sh", "-c", write})
	cmd.Stdin = strings.NewReader(fleetRCContent)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write fleet.rc: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// 2. Wire the rc into .bashrc — touch first so a missing file
	// doesn't break grep, then append the source block only when the
	// marker substring is absent. The heredoc is quoted ('EOF') so the
	// '~' is written literally and expanded by bash at .bashrc read
	// time, not by the staging shell now.
	wire := fmt.Sprintf(`touch %s
if ! grep -qF '%s' %s; then
  cat >> %s <<'EOF'

# Added by fleet-man — source the fleet rc when present.
[ -f ~/%s/%s ] && . ~/%s/%s
EOF
fi`, bashrc, bashrcMarker, bashrc, bashrc, rcSubdir, rcFilename, rcSubdir, rcFilename)
	if out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", wire}).CombinedOutput(); err != nil {
		return fmt.Errorf("wire fleet.rc into .bashrc: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
