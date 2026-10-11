//go:build linux

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// fastPathOff is set once openat2(2) or /proc/self/fd turns out to be
// missing (a kernel before 5.6, a seccomp profile denying openat2, no /proc),
// so later walks go straight to the element walk.
var fastPathOff atomic.Bool

// probeFastPath checks once that openat2 and /proc/self/fd work here.
var probeFastPath = sync.OnceFunc(func() {
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS}
	fd, err := unix.Openat2(unix.AT_FDCWD, ".", &how)
	if err != nil {
		fastPathOff.Store(true)
		return
	}
	defer unix.Close(fd)
	if _, err := os.Stat("/proc/self/fd/" + strconv.Itoa(fd)); err != nil {
		fastPathOff.Store(true)
	}
})

// FastPath reports whether walks use openat2(2), which Linux 5.6 and later
// have unless a sandbox denies it.
func FastPath() bool {
	probeFastPath()
	return !fastPathOff.Load()
}

// openFast opens the folder name in one openat2(2) call that refuses any
// symlink and anything outside the root, then reopens the result as an
// os.Root through /proc/self/fd, which names that exact folder. On any error
// the caller falls back to the element walk.
func (r *Root) openFast(name string) (*dir, error) {
	if !FastPath() {
		return nil, errors.ErrUnsupported
	}
	rootFile, err := r.rawRoot()
	if err != nil {
		return nil, err
	}
	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	var fd int
	for range 3 {
		fd, err = unix.Openat2(int(rootFile.Fd()), name, &how)
		// EAGAIN: a concurrent rename the kernel could not rule out; retry.
		if err != unix.EAGAIN && err != unix.EINTR {
			break
		}
	}
	runtime.KeepAlive(rootFile)
	if err != nil {
		if err == unix.ENOSYS || err == unix.EPERM {
			fastPathOff.Store(true)
		}
		return nil, err
	}
	raw := os.NewFile(uintptr(fd), r.host(name))
	root, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		raw.Close()
		if errors.Is(err, fs.ErrNotExist) {
			fastPathOff.Store(true)
		}
		return nil, err
	}
	return &dir{root: root, path: r.host(name), raw: raw}, nil
}
