package sharesync

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
)

var (
	// ErrStale: content is no longer what was asked for (the file changed at
	// its source, or a hash did not match). A newer change follows.
	ErrStale = errors.New("content changed at its source")
	// ErrNotAttached: a report or fetch for a side the hub does not have.
	ErrNotAttached = errors.New("side is not attached")
	// ErrNewerState: a state file was written by a newer fleet, which this
	// one cannot read; that root is not synced until fleet is upgraded.
	ErrNewerState = errors.New("sync state was written by a newer fleet")
	// ErrTooLarge: content too large to merge in memory.
	ErrTooLarge = errors.New("content too large to merge")
	// ErrClosed: the hub or tree was closed.
	ErrClosed = errors.New("sync is closed")
	// ErrNoSource: no side holds the content an op needs right now.
	ErrNoSource = errors.New("no copy of the content is reachable")
	// errNoSpace marks a write that found too little room.
	errNoSpace = errors.New("no space left")
)

// FlushError is what Flush returns when it could not deliver everything:
// each side still behind, and why.
type FlushError struct {
	// Pending names each side and root still behind, with its pause reason
	// when it is paused.
	Pending []string
}

func (e *FlushError) Error() string {
	return "sync not complete: " + strings.Join(e.Pending, "; ")
}

// classify maps an error from applying an op to its result.
func classify(err error) (ResultStatus, string) {
	switch {
	case err == nil:
		return Applied, ""
	case errors.Is(err, errNoSpace), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return NoSpace, ""
	case errors.Is(err, safefs.ErrReserved):
		return Refused, err.Error()
	case errors.Is(err, ErrStale):
		return Stale, err.Error()
	case errors.Is(err, errLocalChange), errors.Is(err, safefs.ErrChanged),
		errors.Is(err, safefs.ErrSymlink), errors.Is(err, safefs.ErrNotDir),
		errors.Is(err, safefs.ErrNotRegular), errors.Is(err, syscall.ENOTEMPTY),
		errors.Is(err, fs.ErrExist):
		return Changed, err.Error()
	}
	return Failed, err.Error()
}

// errLocalChange: the side holds content its manifest does not describe.
var errLocalChange = errors.New("holds a change not yet reported")

// localChange wraps errLocalChange for path.
func localChange(path string) error {
	return fmt.Errorf("%s %w", path, errLocalChange)
}
