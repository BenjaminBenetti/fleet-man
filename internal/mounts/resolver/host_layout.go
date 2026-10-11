package resolver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
)

// hostLayout prepares a fleet's Layout paths, the folders and files the
// resolver creates under the fleet's mount root (.claude, .codex, .config/gh,
// .augment, .mnt/<path>, files and files/<name>), without following a symlink
// at any of them.
//
// The paths are bind-mounted into every instance of the fleet, so code inside
// one instance can plant a symlink where the host expects a folder or file:
// files/.claude.json -> ~/.bashrc, or (custom mounts nest, /opt over
// /opt/data) .mnt/opt/data -> ~. Followed, the next provision would chmod the
// link's target 0666/0777 and bind-mount it into the fleet. So the mount root
// is walked through internal/safefs, which refuses a symlink at every element
// from the mount root down to a Layout path (SymlinkError here) and never
// lets a symlink swapped in mid-walk redirect an operation. What users keep
// beneath a Layout path is never inspected.
//
// The mount root itself (~/.fleet/workspaces/<fleet>) and everything above it
// belong to the user and are opened following links, so a relocated ~/.fleet
// or fleet folder keeps working.
type hostLayout struct {
	// root is the fleet's mount root; every Layout path is reached through it.
	root *safefs.Root
}

// openHostLayout creates the fleet's mount root when missing (0777 before
// umask, as os.MkdirAll always did for it) and opens it for the walk.
func openHostLayout(dir string) (*hostLayout, error) {
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, err
	}
	root, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		return nil, err
	}
	return &hostLayout{root: root}, nil
}

// Close releases the mount root.
func (l *hostLayout) Close() error {
	return l.root.Close()
}

// ensureDir creates the directory rel (relative to the mount root) and any
// missing parents, then sets rel itself to layoutDirMode (0777). Parents get
// 0777 before umask and no chmod, as os.MkdirAll gave them.
//
// Why 0777 rather than 0700: these directories are bind-mounted into
// containers whose user may have a different UID from the host user
// running fleet (e.g. host root vs container "node" UID 1000). Without
// world-writable perms the container user can't write its own state
// into the mount. Privacy still holds at the parent level: the dirs
// live under ~/.fleet/workspaces/<fleet>/ which is already inside the
// user's HOME, and the agents (Claude/Codex) write their token files
// with their own 0600 perms. The explicit chmod also upgrades existing
// directories that older fleets created at 0700.
func (l *hostLayout) ensureDir(rel string) error {
	name := filepath.ToSlash(rel)
	if err := l.root.MkdirAll(name, 0777); err != nil {
		return refusal(err)
	}
	return refusal(l.root.Chmod(name, layoutDirMode))
}

// ensureFile creates an empty file at rel (relative to the mount root) when
// none exists, and sets it to layoutFileMode (0666) either way. Content of an
// existing file is preserved. 0666 for the UID-mismatch reason on ensureDir:
// the agent that owns the symlinked target inside the container must be able
// to write through to the host file.
func (l *hostLayout) ensureFile(rel string) error {
	name := filepath.ToSlash(rel)
	if err := l.root.MkdirAll(path.Dir(name), 0777); err != nil {
		return refusal(err)
	}
	// O_EXCL never follows a link: an existing entry, a symlink included, is
	// left for Chmod, which refuses a symlink.
	file, err := l.root.OpenFile(name, os.O_RDONLY|os.O_CREATE|os.O_EXCL, layoutFileMode)
	switch {
	case err == nil:
		file.Close()
	case !errors.Is(err, fs.ErrExist):
		return refusal(err)
	}
	return refusal(l.root.Chmod(name, layoutFileMode))
}

// refusal turns safefs's refusals into the resolver's messages: a
// SymlinkError for a symlink, and the folder at fault named for the rest.
func refusal(err error) error {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		return err
	}
	switch {
	case errors.Is(pathErr.Err, safefs.ErrSymlink):
		return &SymlinkError{Path: pathErr.Path}
	case errors.Is(pathErr.Err, safefs.ErrNotDir):
		return fmt.Errorf("%s is not a directory", displayPath(pathErr.Path))
	case errors.Is(pathErr.Err, safefs.ErrChanged):
		return fmt.Errorf("%s changed while fleet was preparing it", displayPath(pathErr.Path))
	}
	return err
}

// displayPath abbreviates the user's home directory in path to "~" for
// messages.
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return path
}
