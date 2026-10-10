package resolver

import "os"

// Modes the resolver enforces on the paths it creates; hostLayout.ensureDir and
// hostLayout.ensureFile apply them and explain why they are world-writable.
const (
	layoutDirMode  os.FileMode = 0777
	layoutFileMode os.FileMode = 0666
)

// LayoutEntry is one path the resolver creates under a fleet's mount root
// (~/.fleet/workspaces/<fleet>): a directory mount's source, the shared
// files directory, or one file inside it.
type LayoutEntry struct {
	// Path is slash-separated and relative to the fleet's mount root. It is
	// never absolute and never contains "..". Missing parents are created as
	// mkdir -p would; the resolver enforces nothing on them.
	Path string
	// Dir is true for a directory, false for a regular file.
	Dir bool
	// Mode is the permission the resolver enforces on Path.
	Mode os.FileMode
}
