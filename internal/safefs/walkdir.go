package safefs

import (
	"errors"
	"io/fs"
	"path"
)

// WalkDir walks the tree at name like fs.WalkDir: fn sees every entry in
// lexical order, folders before their contents, with paths relative to the
// root, and fs.SkipDir and fs.SkipAll work as they do there. It never follows
// a symlink: a symlink is reported as an entry and not descended into. Each
// folder is opened from its parent's handle and checked to be the folder its
// entry described, so an element swapped mid-walk cannot redirect it; fn is
// called with ErrChanged (or ErrSymlink) for such a folder instead.
func (r *Root) WalkDir(name string, fn fs.WalkDirFunc) error {
	info, err := r.Lstat(name)
	if err != nil {
		err = fn(name, nil, err)
	} else {
		err = r.walkTop(name, fs.FileInfoToDirEntry(info), fn)
	}
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

// walkTop opens the folder the walk starts at and walks it.
func (r *Root) walkTop(name string, entry fs.DirEntry, fn fs.WalkDirFunc) error {
	if !entry.IsDir() {
		return fn(name, entry, nil)
	}
	d, err := r.openDir(name, false, 0)
	if err != nil {
		return fn(name, entry, err)
	}
	defer d.close()
	return walkDir(d, name, entry, fn)
}

// walkDir walks the folder d, whose path is name and entry entry. It is
// fs.WalkDir's walkDir over safefs handles.
func walkDir(d *dir, name string, entry fs.DirEntry, fn fs.WalkDirFunc) error {
	if err := fn(name, entry, nil); err != nil {
		if errors.Is(err, fs.SkipDir) {
			return nil
		}
		return err
	}
	entries, err := d.readDir()
	if err != nil {
		// Second call, to report the ReadDir error.
		if err := fn(name, entry, err); err != nil {
			if errors.Is(err, fs.SkipDir) {
				return nil
			}
			return err
		}
	}
	for _, child := range entries {
		childName := path.Join(name, child.Name())
		if !child.IsDir() {
			if err := fn(childName, child, nil); err != nil {
				if errors.Is(err, fs.SkipDir) {
					break
				}
				return err
			}
			continue
		}
		if err := walkChild(d, childName, child, fn); err != nil {
			return err
		}
	}
	return nil
}

// walkChild opens the folder entry of d and walks it.
func walkChild(d *dir, name string, entry fs.DirEntry, fn fs.WalkDirFunc) error {
	info, err := entry.Info()
	if err != nil {
		return fn(name, entry, err)
	}
	child, err := d.openChildAs(entry.Name(), info)
	if err != nil {
		if err := fn(name, entry, err); err != nil && !errors.Is(err, fs.SkipDir) {
			return err
		}
		return nil
	}
	defer child.close()
	return walkDir(child, name, entry, fn)
}
