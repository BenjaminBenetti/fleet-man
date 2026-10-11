//go:build linux

package hostrelay

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// beforeConnect runs between the socket's check and the connect (a seam
// for tests, which swap the name there).
var beforeConnect func()

func dialNoFollow(ctx context.Context, path string) (net.Conn, error) {
	dir, base := filepath.Split(path)
	dfd, err := openDirNoFollow(path, dir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	// The socket itself, as an O_PATH descriptor of what is at the name now
	// (a link there is opened as the link): connect(2) follows a link at a
	// socket's name, so the dial goes through this descriptor, and reaches
	// exactly the socket checked, whatever is put at the name since.
	fd, err := unix.Openat(dfd, base, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, notSocketError(path, statMode(st.Mode))
	}
	if beforeConnect != nil {
		beforeConnect()
	}
	// The kernel resolves /proc/self/fd/N to exactly the socket opened.
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		// The error names the descriptor path; the caller's names the real
		// one.
		var oe *net.OpError
		if errors.As(err, &oe) {
			return nil, &fs.PathError{Op: "dial", Path: path, Err: oe.Err}
		}
		return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	return conn, nil
}

// openDirNoFollow opens dir (path's directory) as an O_PATH descriptor, one
// component at a time from /, refusing a component that is a symbolic link.
func openDirNoFollow(path, dir string) (int, error) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	walked := "/"
	for _, c := range strings.Split(strings.Trim(dir, "/"), "/") {
		if c == "" {
			continue
		}
		walked = filepath.Join(walked, c)
		next, err := unix.Openat(fd, c, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			var st unix.Stat_t
			isLink := unix.Fstatat(fd, c, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
			unix.Close(fd)
			if isLink {
				return -1, linkError(path, walked)
			}
			return -1, &fs.PathError{Op: "dial", Path: path, Err: fmt.Errorf("%s: %w", walked, err)}
		}
		unix.Close(fd)
		fd = next
	}
	return fd, nil
}

// statMode is a stat mode's file type as fs.FileMode bits.
func statMode(m uint32) fs.FileMode {
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		return fs.ModeDir
	case unix.S_IFREG:
		return 0o644
	case unix.S_IFLNK:
		return fs.ModeSymlink
	case unix.S_IFIFO:
		return fs.ModeNamedPipe
	case unix.S_IFCHR:
		return fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		return fs.ModeDevice
	}
	return fs.ModeIrregular
}
