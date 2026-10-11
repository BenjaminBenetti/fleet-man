//go:build darwin || freebsd || netbsd

package sharesync

import (
	"io/fs"
	"syscall"
)

// sysStat returns info's inode, device and change time, where it has them.
func sysStat(info fs.FileInfo) (ino, dev uint64, ctime int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0
	}
	return uint64(st.Ino), uint64(st.Dev), st.Ctimespec.Nano()
}
