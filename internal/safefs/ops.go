package safefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Lstat returns the entry at name without following it. "." is the root.
func (r *Root) Lstat(name string) (fs.FileInfo, error) {
	if err := r.check("lstat", name, true); err != nil {
		return nil, err
	}
	if name == "." {
		info, err := r.root.Lstat(".")
		return info, pathError("lstat", r.dir, err)
	}
	d, leaf, err := r.parent("lstat", name)
	if err != nil {
		return nil, err
	}
	defer d.close()
	info, err := d.root.Lstat(leaf)
	return info, pathError("lstat", d.host(leaf), err)
}

// openFlags are the os.OpenFile flags OpenFile accepts.
const openFlags = os.O_RDONLY | os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL | os.O_TRUNC

// OpenFile opens the regular file name like os.OpenFile, with the flags
// O_RDONLY, O_WRONLY, O_RDWR, O_APPEND, O_CREATE, O_EXCL and O_TRUNC. It
// never follows a symlink and refuses anything but a regular file. A file it
// creates gets perm without set-ID bits, before the umask. An existing file
// is opened O_NONBLOCK (which regular files ignore) and O_NOCTTY.
func (r *Root) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	if flag&^openFlags != 0 {
		return nil, &fs.PathError{Op: "open", Path: r.host(name), Err: fs.ErrInvalid}
	}
	if flag&os.O_CREATE != 0 {
		if err := r.checkAboveReserved("open", name); err != nil {
			return nil, err
		}
	}
	d, leaf, err := r.parent("open", name)
	if err != nil {
		return nil, err
	}
	defer d.close()
	if flag&os.O_CREATE != 0 {
		// O_CREATE|O_EXCL never follows a link: it fails on any entry.
		f, err := d.root.OpenFile(leaf, flag|os.O_EXCL, safeMode(perm))
		if err == nil || flag&os.O_EXCL != 0 || !errors.Is(err, fs.ErrExist) {
			return f, pathError("open", d.host(leaf), err)
		}
	}
	return d.openExisting(leaf, flag&^(os.O_CREATE|os.O_EXCL))
}

// Open opens the regular file name for reading.
func (r *Root) Open(name string) (*os.File, error) {
	return r.OpenFile(name, os.O_RDONLY, 0)
}

// ReadFile returns the content of the regular file name.
func (r *Root) ReadFile(name string) ([]byte, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Mkdir creates the folder name with perm (no set-ID bits, before the umask;
// as with mkdir(2), macOS and the BSDs ignore a sticky bit here, which Chmod
// sets).
func (r *Root) Mkdir(name string, perm fs.FileMode) error {
	d, leaf, err := r.parent("mkdir", name)
	if err != nil {
		return err
	}
	defer d.close()
	return pathError("mkdir", d.host(leaf), d.root.Mkdir(leaf, safeMode(perm)))
}

// MkdirAll creates the folder name and any missing folder above it, each with
// perm (no set-ID bits, before the umask), refusing a symlink or a non-folder
// at any element. Existing folders are left as they are.
func (r *Root) MkdirAll(name string, perm fs.FileMode) error {
	if err := r.check("mkdir", name, true); err != nil {
		return err
	}
	d, err := r.openDir(name, true, perm)
	if err != nil {
		return err
	}
	d.close()
	return nil
}

// Remove removes the file, symlink or empty folder name. A symlink is removed
// itself, never its target.
func (r *Root) Remove(name string) error {
	if err := r.checkReserved("remove", name); err != nil {
		return err
	}
	d, leaf, err := r.parent("remove", name)
	if err != nil {
		return err
	}
	defer d.close()
	return pathError("remove", d.host(leaf), d.root.Remove(leaf))
}

// RemoveAll removes name and everything below it, never following a
// symlink. Like os.RemoveAll, a missing name is not an error; a symlink at a
// folder above name is.
func (r *Root) RemoveAll(name string) error {
	if err := r.checkReserved("removeall", name); err != nil {
		return err
	}
	d, leaf, err := r.parent("removeall", name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.close()
	// os.Root.RemoveAll of a single element opens each folder below it with
	// O_NOFOLLOW|O_DIRECTORY and unlinks from those handles.
	return pathError("removeall", d.host(leaf), d.root.RemoveAll(leaf))
}

// Rename renames oldname to newname, replacing what newname holds when the
// system allows it (a file, a symlink, an empty folder). Neither name is
// followed: a symlink is renamed itself, and one at newname is replaced.
func (r *Root) Rename(oldname, newname string) error {
	for _, name := range []string{oldname, newname} {
		if err := r.check("rename", name, false); err != nil {
			return err
		}
		if err := r.checkReserved("rename", name); err != nil {
			return err
		}
	}
	from, oldleaf, err := r.parent("rename", oldname)
	if err != nil {
		return err
	}
	defer from.close()
	if path.Dir(oldname) == path.Dir(newname) {
		return pathError("rename", from.host(oldleaf), from.root.Rename(oldleaf, path.Base(newname)))
	}
	to, newleaf, err := r.parent("rename", newname)
	if err != nil {
		return err
	}
	defer to.close()
	return pathError("rename", from.host(oldleaf), renameAt(from, oldleaf, to, newleaf))
}

// Symlink creates name as a symlink to target. target is stored as given and
// never resolved by safefs.
func (r *Root) Symlink(target, name string) error {
	if err := r.checkReserved("symlink", name); err != nil {
		return err
	}
	d, leaf, err := r.parent("symlink", name)
	if err != nil {
		return err
	}
	defer d.close()
	return pathError("symlink", d.host(leaf), d.root.Symlink(target, leaf))
}

// Readlink returns the target of the symlink name.
func (r *Root) Readlink(name string) (string, error) {
	d, leaf, err := r.parent("readlink", name)
	if err != nil {
		return "", err
	}
	defer d.close()
	target, err := d.root.Readlink(leaf)
	return target, pathError("readlink", d.host(leaf), err)
}

// Chmod sets the mode of name (permissions and sticky; set-ID bits are
// dropped), refusing a symlink. "." is the root.
func (r *Root) Chmod(name string, mode fs.FileMode) error {
	d, leaf, err := r.leafOrRoot("chmod", name)
	if err != nil {
		return err
	}
	defer d.close()
	if _, err := d.lstatNoLink("chmod", leaf); err != nil {
		return err
	}
	return pathError("chmod", d.host(leaf), chmodAt(d, leaf, safeMode(mode)))
}

// Chtimes sets the access and modification times of name, refusing a
// symlink. Both times are set; "." is the root.
func (r *Root) Chtimes(name string, atime, mtime time.Time) error {
	d, leaf, err := r.leafOrRoot("chtimes", name)
	if err != nil {
		return err
	}
	defer d.close()
	if _, err := d.lstatNoLink("chtimes", leaf); err != nil {
		return err
	}
	return pathError("chtimes", d.host(leaf), chtimesAt(d, leaf, atime, mtime))
}

// Lchown sets the owner of name, a symlink itself when name is one. "." is
// the root.
func (r *Root) Lchown(name string, uid, gid int) error {
	d, leaf, err := r.leafOrRoot("lchown", name)
	if err != nil {
		return err
	}
	defer d.close()
	return pathError("lchown", d.host(leaf), d.root.Lchown(leaf, uid, gid))
}

// ReadDir returns the entries of the folder name sorted by name, with their
// Lstat info. "." is the root.
func (r *Root) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := r.check("readdir", name, true); err != nil {
		return nil, err
	}
	d, err := r.openDir(name, false, 0)
	if err != nil {
		return nil, err
	}
	defer d.close()
	return d.readDir()
}

// OpenRoot opens the folder name as a Root of its own, with the reserved
// paths below it. "." opens the root again.
func (r *Root) OpenRoot(name string) (*Root, error) {
	if err := r.check("open", name, true); err != nil {
		return nil, err
	}
	var reserved []string
	for _, p := range r.reserved {
		if name == "." {
			reserved = append(reserved, p)
		} else if rest, ok := strings.CutPrefix(p, name+"/"); ok {
			reserved = append(reserved, rest)
		}
	}
	if name == "." {
		root, err := r.root.OpenRoot(".")
		if err != nil {
			return nil, pathError("open", r.dir, err)
		}
		return &Root{dir: r.dir, root: root, reserved: reserved, portable: r.portable}, nil
	}
	d, err := r.openDir(name, false, 0)
	if err != nil {
		return nil, err
	}
	if d.raw != nil {
		d.raw.Close()
	}
	return &Root{dir: r.host(name), root: d.root, reserved: reserved, portable: r.portable}, nil
}

// leafOrRoot is parent for operations that also accept "." (the root, reached
// as the element "." of its own folder).
func (r *Root) leafOrRoot(op, name string) (*dir, string, error) {
	if name == "." {
		return r.top(), ".", nil
	}
	return r.parent(op, name)
}
