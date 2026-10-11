package server

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/hostbind"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// checkHostFolders refuses a custom mount host folder that is fleet's own
// state directory, holds it or is inside it (as written, and with links
// followed where they resolve): an instance would bind the daemon's files,
// and a cluster's sync would copy them into pods and back.
func checkHostFolders(folders map[string]string) error {
	own := realPaths(state.FleetDir())
	for _, mount := range slices.Sorted(maps.Keys(folders)) {
		folder := folders[mount]
		for _, f := range realPaths(folder) {
			for _, o := range own {
				if hostbind.Overlaps(f, o) {
					return fmt.Errorf("custom mount %s: host folder %s overlaps fleet's own state (%s); pick another folder", mount, folder, o)
				}
			}
		}
	}
	return nil
}

// hostFolderWarnings warns of each custom mount host folder that is not a
// folder on the daemon host now: an instance is not made until it is.
func hostFolderWarnings(folders map[string]string) []string {
	var out []string
	for _, mount := range slices.Sorted(maps.Keys(folders)) {
		folder := folders[mount]
		info, err := os.Stat(folder)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, fmt.Sprintf("custom mount %s: host folder %s does not exist on the daemon host; create it before making an instance", mount, folder))
		case err != nil:
			out = append(out, fmt.Sprintf("custom mount %s: host folder %s: %v", mount, folder, err))
		case !info.IsDir():
			out = append(out, fmt.Sprintf("custom mount %s: host folder %s is not a folder", mount, folder))
		}
	}
	return out
}

// realPaths is p cleaned, and with its links followed when that differs.
func realPaths(p string) []string {
	out := []string{filepath.Clean(p)}
	if real, err := filepath.EvalSymlinks(p); err == nil && real != out[0] {
		out = append(out, real)
	}
	return out
}
