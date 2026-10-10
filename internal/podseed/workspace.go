package podseed

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// WorkspaceStats is what ExtractWorkspace wrote.
type WorkspaceStats struct {
	Files   int64
	Bytes   int64
	Skipped int64
}

// ExtractWorkspace empties dir and extracts the workspace tar from r into it.
// Every path stays beneath dir (os.Root refuses `..` and symlinks that lead
// out); symlinks are created as they are and never followed. With owner set
// and root privileges, every entry gets owner's ids (the tar's headers
// rewritten to the share uid), else the tar's own when running as root;
// without root, owners stay the job's. Device nodes and FIFOs, which a git
// checkout never has, are skipped and counted. It reads r to its end, so a
// WorkspaceStream's checksum verdict (an error, or io.EOF) is always seen.
func ExtractWorkspace(r io.Reader, dir string, owner *IDs, progress func(WorkspaceStats)) (WorkspaceStats, error) {
	var st WorkspaceStats
	if err := emptyDir(dir); err != nil {
		return st, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return st, err
	}
	defer root.Close()

	isRoot := os.Geteuid() == 0
	chown := func(name string, h *tar.Header, link bool) error {
		if !isRoot {
			return nil
		}
		uid, gid := h.Uid, h.Gid
		if owner != nil {
			uid, gid = owner.UID, owner.GID
		}
		if link {
			return root.Lchown(name, uid, gid)
		}
		return root.Chown(name, uid, gid)
	}
	type dirMeta struct {
		name  string
		mode  fs.FileMode
		mtime time.Time
		hdr   *tar.Header
	}
	var dirs []dirMeta
	lastReport := time.Now()

	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("read the workspace tar: %w", err)
		}
		name, err := tarEntryName(h.Name)
		if err != nil {
			return st, err
		}
		if name != "." {
			if err := ensureParents(root, name, func(dir string) error {
				if isRoot && owner != nil {
					return root.Lchown(dir, owner.UID, owner.GID)
				}
				return nil
			}); err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if name != "." {
				if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
					return st, fmt.Errorf("workspace %s: %w", name, err)
				}
			}
			dirs = append(dirs, dirMeta{name: name, mode: tarMode(h), mtime: h.ModTime, hdr: h})
		case tar.TypeReg:
			if err := replaceEntry(root, name); err != nil {
				return st, err
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			n, err := io.Copy(f, tr)
			if err == nil {
				err = f.Chmod(tarMode(h))
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			if err := chown(name, h, false); err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			// chown clears setuid bits; set the mode again after it.
			if isRoot && tarMode(h)&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
				if err := root.Chmod(name, tarMode(h)); err != nil {
					return st, fmt.Errorf("workspace %s: %w", name, err)
				}
			}
			if err := root.Chtimes(name, h.AccessTime, h.ModTime); err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			st.Files++
			st.Bytes += n
		case tar.TypeSymlink:
			if err := replaceEntry(root, name); err != nil {
				return st, err
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			if err := chown(name, h, true); err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			st.Files++
		case tar.TypeLink:
			target, err := tarEntryName(h.Linkname)
			if err != nil {
				return st, err
			}
			if err := replaceEntry(root, name); err != nil {
				return st, err
			}
			if err := root.Link(target, name); err != nil {
				return st, fmt.Errorf("workspace %s: %w", name, err)
			}
			st.Files++
		default:
			st.Skipped++
		}
		if progress != nil && time.Since(lastReport) > time.Second {
			progress(st)
			lastReport = time.Now()
		}
	}
	// The workspace dir itself belongs to the owner even when the tar has
	// no entry for it, as the workspace folder a bind mount gives docker.
	sawRoot := false
	for _, d := range dirs {
		sawRoot = sawRoot || d.name == "."
	}
	if !sawRoot && isRoot && owner != nil {
		if err := root.Lchown(".", owner.UID, owner.GID); err != nil {
			return st, fmt.Errorf("workspace: %w", err)
		}
	}
	// Directories last, deepest first: creating their entries changed their
	// times, and a read-only mode set earlier would have blocked them.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if fi, err := root.Lstat(d.name); err != nil || !fi.IsDir() {
			continue
		}
		if err := chown(d.name, d.hdr, false); err != nil {
			return st, fmt.Errorf("workspace %s: %w", d.name, err)
		}
		if err := root.Chmod(d.name, d.mode); err != nil {
			return st, fmt.Errorf("workspace %s: %w", d.name, err)
		}
		if err := root.Chtimes(d.name, d.hdr.AccessTime, d.mtime); err != nil {
			return st, fmt.Errorf("workspace %s: %w", d.name, err)
		}
	}
	// The verdict on the whole stream comes after the tar's end marker.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return st, err
	}
	return st, nil
}

// tarEntryName is a tar path as a path relative to the workspace, or an error
// for one that tries to leave it.
func tarEntryName(name string) (string, error) {
	clean := path.Clean(strings.TrimLeft(name, "/"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("workspace tar entry %q leaves the workspace", name)
	}
	return clean, nil
}

// tarMode is a header's permission, setuid, setgid and sticky bits.
func tarMode(h *tar.Header) fs.FileMode {
	m := fs.FileMode(h.Mode & 0o777)
	if h.Mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if h.Mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if h.Mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// replaceEntry removes whatever a tar listing a path twice wrote there first
// (later entries win, as with tar -x), but never a directory with content.
func replaceEntry(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace %s: %w", name, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("workspace %s: a directory is in the way", name)
	}
	return root.Remove(name)
}

// ensureParents creates the missing parent dirs of name, 0755, calling
// created for each (to give it an owner). Entries that list their own dir set
// its mode and times later.
func ensureParents(root *os.Root, name string, created func(dir string) error) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	parts := strings.Split(parent, "/")
	for i := range parts {
		dir := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(dir)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.Mkdir(dir, 0o755); err != nil {
			return err
		}
		if err := created(dir); err != nil {
			return err
		}
	}
	return nil
}

// emptyDir makes dir an existing, empty directory, deleting what an earlier
// attempt left in it.
func emptyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeTree(path.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
