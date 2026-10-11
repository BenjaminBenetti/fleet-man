//go:build unix

package safefs

import (
	"io/fs"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// openGuardFlags go on every open of an existing file, which os.Root makes by
// name: a FIFO swapped in must not block the open, and a terminal must not
// become the daemon's controlling terminal.
const openGuardFlags = syscall.O_NONBLOCK | syscall.O_NOCTTY

// sysMode converts a mode safeMode kept to the system's permission bits.
func sysMode(mode fs.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&fs.ModeSticky != 0 {
		bits |= unix.S_ISVTX
	}
	return bits
}

// chtimesAt sets the times of d's element name without following it.
func chtimesAt(d *dir, name string, atime, mtime time.Time) error {
	f, err := d.file()
	if err != nil {
		return err
	}
	ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	err = unix.UtimesNanoAt(int(f.Fd()), name, ts, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(f)
	return err
}

// renameAt renames from's element oldname to to's element newname. renameat
// follows neither name.
func renameAt(from *dir, oldname string, to *dir, newname string) error {
	src, err := from.file()
	if err != nil {
		return err
	}
	dst, err := to.file()
	if err != nil {
		return err
	}
	err = unix.Renameat(int(src.Fd()), oldname, int(dst.Fd()), newname)
	runtime.KeepAlive(src)
	runtime.KeepAlive(dst)
	return err
}
