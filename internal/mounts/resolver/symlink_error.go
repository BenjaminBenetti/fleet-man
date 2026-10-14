package resolver

import "errors"

// SymlinkError reports a symlink where the resolver expects one of a fleet's
// Layout paths (a mount folder, a folder above one, or a shared file), which
// it refuses rather than follow. Resolve wraps it with the mount it belongs
// to and the fix.
type SymlinkError struct {
	// Path is the absolute host path of the symlink.
	Path string
}

func (e *SymlinkError) Error() string {
	return displayPath(e.Path) + " is a symlink"
}

// symlinkFix returns "; " + fix when err is a SymlinkError, for ending its
// message with the fix, and "" for any other error.
func symlinkFix(err error, fix string) string {
	if !errors.As(err, new(*SymlinkError)) {
		return ""
	}
	return "; " + fix
}
