//go:build unix && !linux

package safefs

import (
	"io/fs"
	"runtime"

	"golang.org/x/sys/unix"
)

// chmodAt sets the mode of d's element name without following it: macOS and
// the BSDs honour AT_SYMLINK_NOFOLLOW (a link swapped in gets its own mode
// changed, or the call fails, and nothing is followed).
func chmodAt(d *dir, name string, mode fs.FileMode) error {
	f, err := d.file()
	if err != nil {
		return err
	}
	err = unix.Fchmodat(int(f.Fd()), name, sysMode(mode), unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(f)
	return err
}
