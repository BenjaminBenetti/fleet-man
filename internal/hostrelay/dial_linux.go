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

// maxSunPath is the longest path a unix socket address holds (sun_path, its
// NUL excluded).
const maxSunPath = 107

func dialNoFollow(ctx context.Context, path string) (net.Conn, error) {
	dir, base := filepath.Split(path)
	fd, err := openDirNoFollow(path, dir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, notSocketError(path, statMode(st.Mode))
	}
	// The directory the walk opened, by its descriptor: the kernel resolves
	// /proc/self/fd/N to exactly it.
	addr := fmt.Sprintf("/proc/self/fd/%d/%s", fd, base)
	if len(addr) > maxSunPath {
		return nil, &fs.PathError{Op: "dial", Path: path, Err: fmt.Errorf("the socket's name is longer than a unix socket address holds")}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", addr)
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
