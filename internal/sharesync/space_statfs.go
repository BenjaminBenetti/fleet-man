//go:build linux || darwin || freebsd

package sharesync

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// diskSpace reports the space of the filesystem holding dir.
func diskSpace(dir string) (Space, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return Space{}, fmt.Errorf("statfs %s: %w", dir, err)
	}
	bs := int64(st.Bsize)
	return Space{Total: int64(st.Blocks) * bs, Free: int64(st.Bavail) * bs}, nil
}
