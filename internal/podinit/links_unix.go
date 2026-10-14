//go:build unix

package podinit

import (
	"os"
	"syscall"
)

// ownLikeTarget gives the link at path the owner and group of its target,
// when it can.
func ownLikeTarget(path, target string) {
	fi, err := os.Stat(target)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}
