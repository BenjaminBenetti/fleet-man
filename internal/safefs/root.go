// Package safefs reads and writes a directory tree that other parties change
// at the same time, without ever leaving the tree or following a symlink in
// it.
//
// sharesync's hub applies changes made in cluster pods to a fleet's host
// folders (~/.fleet/workspaces/<fleet>/...), and the fleet's local docker
// instances bind-mount those same folders and can rewrite them at any moment:
// an element of a path the hub is about to write can become a symlink between
// two system calls. Every operation on a Root therefore:
//
//   - stays beneath the root. Each folder on the way is held open as an
//     os.Root, so nothing resolved from it can reach outside, mid-race
//     included.
//   - never follows a symlink, at any element including the last. A symlink
//     is refused (ErrSymlink), and an entry replaced between safefs checking
//     it and using it is refused too (ErrChanged), with nothing done through
//     the replacement. Operations on a link itself (Lstat, Readlink, Remove,
//     RemoveAll, Rename, Lchown) act on the link.
//   - never sets a set-user-ID or set-group-ID bit: they are dropped from
//     every mode it is given. The files belong to the daemon's user, and
//     content arriving from a pod must not plant a set-ID program on the
//     daemon host. (A folder made inside a set-group-ID folder still
//     inherits that bit from the kernel, which only steers group ownership.)
//     Only Chmod sets a sticky bit, and only on a folder.
//
// The walk to an element's folder is portable: one os.Root per folder, each
// element Lstat-ed and refused when it is a symlink or not a folder, and each
// opened folder checked to be the one the Lstat saw. On Linux, openat2(2)
// with RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS does the whole walk in one call
// when the kernel has it. The last element is then reached from that folder
// with a system call that does not follow it (fstatat, unlinkat, renameat,
// symlinkat, fchownat, utimensat with AT_SYMLINK_NOFOLLOW, O_CREAT|O_EXCL; a
// chmod through an O_PATH handle on Linux, which needs neither fchmodat2 nor
// access to the file, and fchmodat with AT_SYMLINK_NOFOLLOW elsewhere) or,
// for opening an existing file, an open checked against the Lstat. A file
// is replaced through a temp file created O_CREAT|O_EXCL in its own folder
// and renamed over it there.
//
// The root folder itself, and everything above it, belong to the caller: Open
// follows a symlink to it, so a relocated ~/.fleet keeps working.
package safefs

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Options configure a Root.
type Options struct {
	// Reserved are paths fleet itself prepares beneath the root, such as a
	// fleet's Layout paths (internal/mounts/resolver): slash-separated and
	// relative to the root, in the io/fs form. The Root never removes,
	// renames or replaces one, or a folder above one, never creates a
	// symlink there, and never creates a file where a folder above one
	// belongs (ErrReserved). Writing a reserved file's content is allowed.
	// An entry that exists is recognised under any name the filesystem
	// gives it (another case, on a case-insensitive one) when it would be
	// removed, renamed or renamed over.
	Reserved []string
}

// Root is a directory tree safefs works in. Names passed to its methods are
// slash-separated and relative to the root, in the io/fs form (fs.ValidPath):
// "." for the root itself, otherwise no ".", ".." or empty elements and no
// leading or trailing slash. A Root is safe for concurrent use.
type Root struct {
	// dir is the root's host path.
	dir      string
	root     *os.Root
	reserved []string
	// portable skips the Linux fast path; tests run both walks.
	portable bool

	// raw is the root opened as a file for the *at system calls os.Root has
	// no method for (the fast path's openat2); opened on first use.
	rawMu sync.Mutex
	raw   *os.File
}

// Open opens dir, following it when it is a symlink, as a Root.
func Open(dir string, opts Options) (*Root, error) {
	reserved := make([]string, 0, len(opts.Reserved))
	for _, name := range opts.Reserved {
		if name == "." || !fs.ValidPath(name) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
		}
		reserved = append(reserved, name)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Root{dir: dir, root: root, reserved: reserved}, nil
}

// Name returns the host path the Root was opened with.
func (r *Root) Name() string {
	return r.dir
}

// Close releases the Root. Files it opened stay open.
func (r *Root) Close() error {
	r.rawMu.Lock()
	if r.raw != nil {
		r.raw.Close()
		r.raw = nil
	}
	r.rawMu.Unlock()
	return r.root.Close()
}

// rawRoot returns the root opened as a file, opening it on first use.
func (r *Root) rawRoot() (*os.File, error) {
	r.rawMu.Lock()
	defer r.rawMu.Unlock()
	if r.raw == nil {
		f, err := r.root.Open(".")
		if err != nil {
			return nil, err
		}
		r.raw = f
	}
	return r.raw, nil
}

// host returns the host path of name.
func (r *Root) host(name string) string {
	return filepath.Join(r.dir, filepath.FromSlash(name))
}

// top returns the root's own folder as a dir. Closing it leaves the Root open.
func (r *Root) top() *dir {
	return &dir{root: r.root, path: r.dir, borrowed: true}
}

// check refuses a name that is not a valid path. allowRoot accepts "." for
// operations that may act on the root folder itself.
func (r *Root) check(op, name string, allowRoot bool) error {
	if !fs.ValidPath(name) || name == "." && !allowRoot {
		return &fs.PathError{Op: op, Path: r.host(name), Err: fs.ErrInvalid}
	}
	return nil
}

// guardReserved refuses an operation that would remove name, rename it or
// rename over it when name is reserved or a folder above a reserved path.
// Names are compared as given and then, when name exists, by identity with
// each reserved path or folder above one of the same depth: on a
// case-insensitive filesystem (macOS's by default) ".mnt/OPT/Data" is the
// entry ".mnt/opt/data" names. A hard link to a reserved file is refused too.
func (r *Root) guardReserved(op, name string) error {
	if err := r.check(op, name, false); err != nil {
		return err
	}
	if err := r.checkReserved(op, name); err != nil {
		return err
	}
	var target fs.FileInfo
	for _, guarded := range r.guardedAt(strings.Count(name, "/") + 1) {
		if target == nil {
			info, err := r.Lstat(name)
			if err != nil {
				// Nothing there to be a reserved entry; the operation
				// itself reports what is wrong.
				return nil
			}
			target = info
		}
		if info, err := r.Lstat(guarded); err == nil && os.SameFile(info, target) {
			return &fs.PathError{Op: op, Path: r.host(name), Err: ErrReserved}
		}
	}
	return nil
}

// guardedAt returns, once each, the reserved paths and folders above them
// that have depth elements.
func (r *Root) guardedAt(depth int) []string {
	var guarded []string
	for _, reserved := range r.reserved {
		elems := strings.Split(reserved, "/")
		if len(elems) < depth {
			continue
		}
		if p := strings.Join(elems[:depth], "/"); !slices.Contains(guarded, p) {
			guarded = append(guarded, p)
		}
	}
	return guarded
}

// checkReserved refuses an operation that would remove, rename over or put a
// symlink at name when name is reserved or a folder above a reserved path,
// comparing the names as given.
func (r *Root) checkReserved(op, name string) error {
	for _, reserved := range r.reserved {
		if reserved == name || strings.HasPrefix(reserved, name+"/") {
			return &fs.PathError{Op: op, Path: r.host(name), Err: ErrReserved}
		}
	}
	return nil
}

// checkAboveReserved refuses an operation that would create a file at name
// when name is a folder above a reserved path. A reserved file's own content
// may be written.
func (r *Root) checkAboveReserved(op, name string) error {
	for _, reserved := range r.reserved {
		if strings.HasPrefix(reserved, name+"/") {
			return &fs.PathError{Op: op, Path: r.host(name), Err: ErrReserved}
		}
	}
	return nil
}
