//go:build linux

package safefs

import (
	"io/fs"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// chmodAt sets the mode of d's element name without following it. Linux's
// fchmodat ignores AT_SYMLINK_NOFOLLOW and fchmodat2 is 6.6+ (and a seccomp
// profile may answer it EPERM), so the element is opened O_PATH|O_NOFOLLOW,
// which needs no access to the file, and chmodded through its /proc/self/fd
// entry, which names that exact inode. Without /proc it is opened for reading
// and fchmodded instead.
func chmodAt(d *dir, name string, mode fs.FileMode) error {
	f, err := d.file()
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(f)
	fd, err := unix.Openat(int(f.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrSymlink
	}
	err = unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), sysMode(mode))
	if err != unix.ENOENT {
		return err
	}
	return fchmodOpened(int(f.Fd()), name, &st, mode)
}

// fchmodOpened is chmodAt without /proc: it opens name for reading (no
// follow, no blocking, no controlling terminal), checks it is the inode st
// describes, and fchmods it.
func fchmodOpened(dirfd int, name string, st *unix.Stat_t, mode fs.FileMode) error {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino {
		return ErrChanged
	}
	return unix.Fchmod(fd, sysMode(mode))
}
