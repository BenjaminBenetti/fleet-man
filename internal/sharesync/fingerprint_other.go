//go:build !(linux || openbsd || dragonfly || darwin || freebsd || netbsd)

package sharesync

import "io/fs"

// sysStat has nothing beyond fs.FileInfo here: fingerprints are size, time
// and mode.
func sysStat(fs.FileInfo) (ino, dev uint64, ctime int64) {
	return 0, 0, 0
}
