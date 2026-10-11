package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// dir is one folder beneath a Root, held open as an os.Root, so nothing
// resolved from it can leave it. Methods take single path elements.
type dir struct {
	root *os.Root
	// path is the folder's host path, for errors.
	path string
	// borrowed marks the Root's own folder, which close leaves open.
	borrowed bool
	// raw is the folder opened as a file for the *at system calls os.Root has
	// no no-follow form of; opened on first use, closed with the dir.
	raw *os.File
}

// close releases the folder.
func (d *dir) close() {
	if d.raw != nil {
		d.raw.Close()
	}
	if !d.borrowed {
		d.root.Close()
	}
}

// host returns the host path of the element name in d.
func (d *dir) host(name string) string {
	if name == "." {
		return d.path
	}
	return filepath.Join(d.path, name)
}

// file returns the folder opened as a file, for *at system calls. "." is the
// folder itself, never a link.
func (d *dir) file() (*os.File, error) {
	if d.raw == nil {
		f, err := d.root.Open(".")
		if err != nil {
			return nil, err
		}
		d.raw = f
	}
	return d.raw, nil
}

// openChild opens d's element name as a folder, creating it with perm when it
// is missing and create is set. It refuses a symlink or a non-folder.
func (d *dir) openChild(name string, create bool, perm fs.FileMode) (*dir, error) {
	info, err := d.root.Lstat(name)
	if create && errors.Is(err, fs.ErrNotExist) {
		// ErrExist: a concurrent creator won; the Lstat below sees its folder.
		if err := d.root.Mkdir(name, safeMode(perm)); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, pathError("mkdir", d.host(name), err)
		}
		info, err = d.root.Lstat(name)
	}
	if err != nil {
		return nil, pathError("lstat", d.host(name), err)
	}
	return d.openChildAs(name, info)
}

// openChildAs opens d's element name, which an Lstat reported as info, as a
// folder. os.Root's open follows a symlink swapped in since the Lstat (within
// d), so the folder opened must be the one the Lstat saw.
func (d *dir) openChildAs(name string, info fs.FileInfo) (*dir, error) {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, pathError("open", d.host(name), ErrSymlink)
	case !info.IsDir():
		return nil, pathError("open", d.host(name), ErrNotDir)
	}
	child, err := d.root.OpenRoot(name)
	if err != nil {
		if now, lerr := d.root.Lstat(name); lerr == nil && now.Mode()&fs.ModeSymlink != 0 {
			err = ErrSymlink
		}
		return nil, pathError("open", d.host(name), err)
	}
	opened, err := child.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = ErrChanged
	}
	if err != nil {
		child.Close()
		return nil, pathError("open", d.host(name), err)
	}
	return &dir{root: child, path: d.host(name)}, nil
}

// openExisting opens d's existing regular file name with flag (no O_CREATE),
// refusing a symlink or anything but a regular file. os.Root's open follows a
// symlink swapped in since the Lstat, so the open carries nothing that acts
// before the check (O_TRUNC is applied after it, O_NONBLOCK keeps a FIFO from
// blocking it, O_NOCTTY keeps a terminal from becoming the daemon's), and the
// file opened must be the one the Lstat saw.
func (d *dir) openExisting(name string, flag int) (*os.File, error) {
	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, pathError("open", d.host(name), err)
	}
	if err := regular(info); err != nil {
		return nil, pathError("open", d.host(name), err)
	}
	f, err := d.root.OpenFile(name, flag&^os.O_TRUNC|openGuardFlags, 0)
	if err != nil {
		return nil, pathError("open", d.host(name), err)
	}
	opened, err := f.Stat()
	if err == nil && !os.SameFile(info, opened) {
		err = ErrChanged
	}
	if err == nil && flag&os.O_TRUNC != 0 {
		err = f.Truncate(0)
	}
	if err != nil {
		f.Close()
		return nil, pathError("open", d.host(name), err)
	}
	return f, nil
}

// lstatNoLink Lstats d's element name and refuses a symlink.
func (d *dir) lstatNoLink(op, name string) (fs.FileInfo, error) {
	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, pathError(op, d.host(name), err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, pathError(op, d.host(name), ErrSymlink)
	}
	return info, nil
}

// readDir returns d's entries sorted by name, each Lstat-ed through d (an
// entry removed meanwhile is left out).
func (d *dir) readDir() ([]fs.DirEntry, error) {
	f, err := d.root.Open(".")
	if err != nil {
		return nil, pathError("readdir", d.path, err)
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, pathError("readdir", d.path, err)
	}
	slices.Sort(names)
	entries := make([]fs.DirEntry, 0, len(names))
	for _, name := range names {
		info, err := d.root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, pathError("lstat", d.host(name), err)
		}
		entries = append(entries, fs.FileInfoToDirEntry(info))
	}
	return entries, nil
}

// openDir opens the folder name beneath r, refusing a symlink or a non-folder
// at any element. create makes missing elements with perm.
func (r *Root) openDir(name string, create bool, perm fs.FileMode) (*dir, error) {
	if name == "." {
		return r.top(), nil
	}
	if !r.portable {
		// Any failure (a missing element to create, a symlink to name) falls
		// back to the element walk, which creates or names the element at fault.
		if d, err := r.openFast(name); err == nil {
			return d, nil
		}
	}
	d := r.top()
	for _, elem := range strings.Split(name, "/") {
		child, err := d.openChild(elem, create, perm)
		d.close()
		if err != nil {
			return nil, err
		}
		d = child
	}
	return d, nil
}

// parent opens the folder holding name and returns it with name's last
// element. name must be valid and not ".".
func (r *Root) parent(op, name string) (*dir, string, error) {
	if err := r.check(op, name, false); err != nil {
		return nil, "", err
	}
	d, err := r.openDir(path.Dir(name), false, 0)
	if err != nil {
		return nil, "", err
	}
	return d, path.Base(name), nil
}

// regular refuses anything but a regular file.
func regular(info fs.FileInfo) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return ErrSymlink
	case !info.Mode().IsRegular():
		return ErrNotRegular
	}
	return nil
}

// safeMode keeps the mode bits safefs lets an entry have: permissions and
// sticky, never set-user-ID or set-group-ID.
func safeMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSticky)
}
