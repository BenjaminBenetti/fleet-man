package safefs

import (
	"errors"
	"io/fs"
	"os"
)

// The refusals safefs reports. Each comes wrapped in an *fs.PathError whose
// Path is the host path of the element at fault, which need not be the path
// the operation named: writing a/b/c with a symlink at a reports a.
var (
	// ErrSymlink: a symlink where safefs needs a real folder or file.
	ErrSymlink = errors.New("is a symlink")
	// ErrNotDir: something other than a folder where the path needs one.
	ErrNotDir = errors.New("is not a directory")
	// ErrNotRegular: something other than a regular file (a folder, a FIFO, a
	// device or a socket) where the operation needs one.
	ErrNotRegular = errors.New("is not a regular file")
	// ErrChanged: the entry was replaced between safefs checking it and using
	// it. Nothing was done through the replacement; a retry sees what is
	// there now.
	ErrChanged = errors.New("changed while fleet was using it")
	// ErrReserved: the operation would remove, rename, replace or put a
	// symlink at a reserved path or a folder above one (see Options).
	ErrReserved = errors.New("is reserved for fleet")
)

// pathError wraps err for the host path at fault, unwrapping the os.Root
// error inside it so the path is never reported twice (os.Root names paths
// by its own handles, which here can be /proc/self/fd entries).
func pathError(op, hostPath string, err error) error {
	if err == nil {
		return nil
	}
	return &fs.PathError{Op: op, Path: hostPath, Err: cause(err)}
}

// cause strips the path wrappers os puts around an errno, leaving it or
// safefs's own sentinel.
func cause(err error) error {
	for {
		var pathErr *fs.PathError
		var linkErr *os.LinkError
		switch {
		case errors.As(err, &pathErr):
			err = pathErr.Err
		case errors.As(err, &linkErr):
			err = linkErr.Err
		default:
			return err
		}
	}
}
