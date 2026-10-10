package resolver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
// link's target 0666/0777 and bind-mount it into the fleet. So every
// component from the mount root down to a Layout path is Lstat-ed and refused
// when it is a symlink (SymlinkError), and it is walked one os.Root per
// directory, each checked to be the same inode the Lstat saw, so a symlink
// swapped in mid-walk can never redirect an operation outside the folder that
// holds it. What users keep beneath a Layout path is never inspected.
//
// The mount root itself (~/.fleet/workspaces/<fleet>) and everything above it
// belong to the user and are opened following links, so a relocated ~/.fleet
// or fleet folder keeps working.
type hostLayout struct {
	// dir is the fleet's mount root on the host.
	dir string
	// root is dir opened as an os.Root; every Layout path is reached through it.
	root *os.Root
}

// openHostLayout creates the fleet's mount root when missing (0777 before
// umask, as os.MkdirAll always did for it) and opens it for the walk.
func openHostLayout(dir string) (*hostLayout, error) {
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &hostLayout{dir: dir, root: root}, nil
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
	dir, err := l.openDir(rel)
	if err != nil {
		return err
	}
	defer dir.Close()
	// "." is the walked directory itself, never a link.
	return dir.Chmod(".", layoutDirMode)
}

// ensureFile creates an empty file at rel (relative to the mount root) when
// none exists, and sets it to layoutFileMode (0666) either way. Content of an
// existing file is preserved. 0666 for the UID-mismatch reason on ensureDir:
// the agent that owns the symlinked target inside the container must be able
// to write through to the host file.
func (l *hostLayout) ensureFile(rel string) error {
	dir, err := l.openDir(filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer dir.Close()

	name := filepath.Base(rel)
	// O_EXCL never follows a link, so a symlink planted since the Lstat makes
	// the create fail rather than write through it.
	info, err := lstatOrCreate(dir, name, func() error {
		file, err := dir.OpenFile(name, os.O_RDONLY|os.O_CREATE|os.O_EXCL, layoutFileMode)
		if err != nil {
			return err
		}
		return file.Close()
	})
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return &SymlinkError{Path: filepath.Join(l.dir, rel)}
	}
	// fchmodat(AT_SYMLINK_NOFOLLOW) inside dir: a link swapped in after the
	// Lstat can only resolve within dir, which holds nothing but these files.
	return dir.Chmod(name, layoutFileMode)
}

// openDir walks rel from the mount root one component at a time, creating a
// missing component (0777 before umask) and refusing one that is a symlink or
// not a directory, and returns rel opened as its own os.Root.
func (l *hostLayout) openDir(rel string) (*os.Root, error) {
	parent := l.root
	hostPath := l.dir
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		hostPath = filepath.Join(hostPath, name)
		child, err := openLayoutDir(parent, name, hostPath)
		if parent != l.root {
			parent.Close()
		}
		if err != nil {
			return nil, err
		}
		parent = child
	}
	return parent, nil
}

// openLayoutDir opens parent's entry name, whose host path is hostPath, as an
// os.Root after creating it when missing. It refuses a symlink or a
// non-directory, and an entry that changed between the Lstat and the open.
func openLayoutDir(parent *os.Root, name, hostPath string) (*os.Root, error) {
	info, err := lstatOrCreate(parent, name, func() error {
		return parent.Mkdir(name, 0777)
	})
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, &SymlinkError{Path: hostPath}
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", displayPath(hostPath))
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = fmt.Errorf("%s changed while fleet was preparing it", displayPath(hostPath))
	}
	if err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

// lstatOrCreate returns the Lstat of name in dir, first running create when
// name is missing. create failing with ErrExist is not an error: another
// provision of the same fleet created the entry first, and the second Lstat
// reports whatever is there now.
func lstatOrCreate(dir *os.Root, name string, create func() error) (fs.FileInfo, error) {
	info, err := dir.Lstat(name)
	if !errors.Is(err, fs.ErrNotExist) {
		return info, err
	}
	if err := create(); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return dir.Lstat(name)
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
