package safefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"strings"
	"syscall"
	"time"
)

// TempPrefix starts the name of every temp file WriteFile makes beside its
// target. A scanner of a tree safefs writes into skips names IsTemp reports.
const TempPrefix = ".fleet-safefs-"

// tempSuffix ends every temp file name.
const tempSuffix = ".tmp"

// IsTemp reports whether name, a single path element, is a WriteFile temp
// file.
func IsTemp(name string) bool {
	return strings.HasPrefix(name, TempPrefix) && strings.HasSuffix(name, tempSuffix)
}

// tempName returns a fresh temp file name.
func tempName() string {
	return fmt.Sprintf("%s%016x%s", TempPrefix, rand.Uint64(), tempSuffix)
}

// WriteFile replaces the regular file name with the content of data and
// perm's permission bits (not masked by the umask), atomically: it writes a
// temp file created O_CREAT|O_EXCL in name's folder and renames it over name
// there, so a reader sees the old file or the new one, never part of either.
// What name held (a file, or a symlink, which is replaced and never followed)
// is gone; a folder at name is refused. The parent folder must exist. Like
// internal/atomicfile it does not fsync.
//
// It returns the Lstat of the file it wrote. ErrChanged means another party
// replaced the temp file before the rename (nothing is renamed into place)
// or the result right after it; a retry writes again.
func (r *Root) WriteFile(name string, data io.Reader, perm fs.FileMode) (fs.FileInfo, error) {
	return r.ReplaceFile(name, data, perm, time.Time{}, nil)
}

// ReplaceFile is WriteFile with a modification time and a precondition, for
// a writer that must not replace content it has not seen (sharesync applying
// a change over a file a local docker instance may have just rewritten). A
// non-zero mtime is set on the temp file (and its access time with it)
// before the rename, so the file never shows another time and a write made
// to it after the rename shows as a new time. Once data is in the temp file,
// and just before the rename, expect is called with lstat, which reports
// what name holds at the moment it is called (nil, nil when nothing), Lstat-
// ed through the folder the rename goes into: a caller that serializes with
// other writers takes its lock first and then looks. An error from expect
// leaves name as it is, removes the temp file and is returned as it is. Only
// the rename itself happens after the check. A nil expect skips the check.
func (r *Root) ReplaceFile(name string, data io.Reader, perm fs.FileMode, mtime time.Time, expect func(lstat func() (fs.FileInfo, error)) error) (fs.FileInfo, error) {
	if err := r.checkAboveReserved("write", name); err != nil {
		return nil, err
	}
	d, leaf, err := r.parent("write", name)
	if err != nil {
		return nil, err
	}
	defer d.close()
	if info, err := d.root.Lstat(leaf); err == nil && info.IsDir() {
		return nil, pathError("write", d.host(leaf), syscall.EISDIR)
	}

	tmp, f, err := createTemp(d)
	if err != nil {
		return nil, pathError("write", d.host(leaf), err)
	}
	renamed := false
	defer func() {
		if !renamed {
			d.root.Remove(tmp)
		}
	}()
	written, err := fillTemp(f, data, perm)
	if err != nil {
		return nil, pathError("write", d.host(leaf), err)
	}
	if !mtime.IsZero() {
		// By name, never following it: a temp file swapped meanwhile is
		// caught by the identity check below.
		if err := chtimesAt(d, tmp, mtime, mtime); err != nil {
			return nil, pathError("write", d.host(leaf), err)
		}
	}
	// A party that can write the folder can swap the temp file for its own
	// entry before the rename. That gains it nothing it could not do to name
	// directly, but the write would then not be fleet's, so it is refused.
	if now, err := d.root.Lstat(tmp); err != nil || !os.SameFile(now, written) {
		return nil, pathError("write", d.host(leaf), ErrChanged)
	}
	if expect != nil {
		lstat := func() (fs.FileInfo, error) {
			current, err := d.root.Lstat(leaf)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return nil, nil
			case err != nil:
				return nil, pathError("write", d.host(leaf), err)
			}
			return current, nil
		}
		if err := expect(lstat); err != nil {
			return nil, err
		}
	}
	if err := d.root.Rename(tmp, leaf); err != nil {
		return nil, pathError("write", d.host(leaf), err)
	}
	renamed = true
	info, err := d.root.Lstat(leaf)
	if err == nil && !os.SameFile(info, written) {
		err = ErrChanged
	}
	if err != nil {
		return nil, pathError("write", d.host(leaf), err)
	}
	return info, nil
}

// createTemp creates a fresh temp file in d, retrying on a name that exists.
func createTemp(d *dir) (string, *os.File, error) {
	for range 10 {
		name := tempName()
		f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return name, f, err
	}
	return "", nil, fs.ErrExist
}

// fillTemp writes data into the temp file f, sets perm on it through the file
// (never by name) and closes it, returning its Stat.
func fillTemp(f *os.File, data io.Reader, perm fs.FileMode) (fs.FileInfo, error) {
	_, err := io.Copy(f, data)
	if err == nil {
		err = f.Chmod(createMode(perm))
	}
	var info fs.FileInfo
	if err == nil {
		info, err = f.Stat()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return info, err
}
