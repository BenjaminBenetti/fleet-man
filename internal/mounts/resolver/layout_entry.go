package resolver

import (
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// Modes the resolver enforces on the paths it creates; hostLayout.ensureDir and
// hostLayout.ensureFile apply them and explain why they are world-writable.
const (
	layoutDirMode  os.FileMode = 0777
	layoutFileMode os.FileMode = 0666
)

// LayoutEntry is one path the resolver creates under a fleet's mount root
// (~/.fleet/workspaces/<fleet>): a directory mount's source, the shared
// files directory, or one file inside it. A file's Seed is its
// fileMountSpec's seedContent, which the host file gets only once its
// symlink is made (applyMountSymlinks); a backend replaying the layout
// elsewhere creates the file with it.
type LayoutEntry = backend.LayoutEntry
