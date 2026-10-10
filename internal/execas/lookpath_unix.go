//go:build unix

package execas

import (
	"io/fs"
	"slices"
	"syscall"
)

// mayExecute is execute permission for id from fi's mode bits, as the kernel
// checks it: root needs any execute bit, everyone else the owner's, the
// group's (primary or supplementary) or other's.
func mayExecute(fi fs.FileInfo, id Identity) bool {
	mode := fi.Mode().Perm()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return mode&0o111 != 0
	}
	switch {
	case id.UID == 0:
		return mode&0o111 != 0
	case st.Uid == id.UID:
		return mode&0o100 != 0
	case st.Gid == id.GID || slices.Contains(id.Groups, st.Gid):
		return mode&0o010 != 0
	}
	return mode&0o001 != 0
}
