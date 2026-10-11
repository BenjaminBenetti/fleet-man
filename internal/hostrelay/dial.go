package hostrelay

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"time"
)

// DialTimeout bounds a Dial that ctx leaves open.
const DialTimeout = 5 * time.Second

// ErrLink is a recorded socket path with a symbolic link on the way: the path
// was resolved when it was recorded, so a link there now is someone's
// redirect, which Dial does not follow.
var ErrLink = errors.New("a symbolic link is on the way")

// ErrNotSocket is a recorded socket path that holds something else now.
var ErrNotSocket = errors.New("not a unix socket")

// Dial connects to the unix socket at path on this host, a recorded Source:
// absolute and clean, every component of it what it was when it was recorded
// (a directory, never a symbolic link) and a socket at its end. On Linux the
// connection is made through the socket it checked (an O_PATH descriptor of
// it, opened component by component with O_NOFOLLOW), so nothing can be
// swapped in between the check and the connect; elsewhere each component is
// checked with lstat just before.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, &fs.PathError{Op: "dial", Path: path, Err: errors.New("not a clean absolute path")}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DialTimeout)
		defer cancel()
	}
	conn, err := dialNoFollow(ctx, path)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, err
		}
		return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	return conn, nil
}

// linkError is the error for a symbolic link found at component on the way
// to path.
func linkError(path, component string) error {
	return &fs.PathError{Op: "dial", Path: path, Err: fmt.Errorf("%w: %s (resolved when the socket was recorded; fleet does not follow a link that appeared since)", ErrLink, component)}
}

// notSocketError is the error for a path that holds mode instead of a socket.
func notSocketError(path string, mode fs.FileMode) error {
	what := "nothing usable"
	switch {
	case mode.IsDir():
		what = "a directory"
	case mode.IsRegular():
		what = "a regular file"
	case mode&fs.ModeSymlink != 0:
		return linkError(path, path)
	case mode != 0:
		what = "a " + mode.Type().String() + " file"
	}
	return &fs.PathError{Op: "dial", Path: path, Err: fmt.Errorf("%w: it is %s", ErrNotSocket, what)}
}
