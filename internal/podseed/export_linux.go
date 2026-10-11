//go:build linux

package podseed

import (
	"fmt"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// NestedMounts lists the mounts strictly beneath dir, from this process's
// mount table: what an export of dir sends as empty directories.
func NestedMounts(dir string) ([]string, error) {
	dir = filepath.Clean(dir)
	mounts, err := sourceMounts(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(mounts))
	for m := range mounts {
		if dir == "/" {
			out = append(out, m)
		} else {
			out = append(out, dir+m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DiskUsage is the size of the filesystem holding path and the room left on
// it for an unprivileged writer (what a reseed copying as root can count on
// with the reserve to spare).
func DiskUsage(path string) (DiskSpace, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskSpace{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bs := int64(st.Bsize)
	return DiskSpace{Total: int64(st.Blocks) * bs, Free: int64(st.Bavail) * bs}, nil
}
