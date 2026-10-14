//go:build linux

package podseed

import (
	"bytes"
	"errors"

	"golang.org/x/sys/unix"
)

// capabilityXattr holds a file's capabilities (setcap).
const capabilityXattr = "security.capability"

// selinuxXattr is the SELinux label: the node's to assign (the pod's MCS
// level and MountOption relabeling), never the image's to impose.
const selinuxXattr = "security.selinux"

// skipXattr reports whether an attribute is never copied.
func skipXattr(name string) bool { return name == selinuxXattr }

// listXattrs lists the extended attributes of fd (when ≥ 0) or of path itself
// (a symlink is not followed).
func listXattrs(path string, fd int) ([]string, error) {
	size := 1024
	for {
		buf := make([]byte, size)
		var n int
		var err error
		if fd >= 0 {
			n, err = unix.Flistxattr(fd, buf)
		} else {
			n, err = unix.Llistxattr(path, buf)
		}
		if errors.Is(err, unix.ERANGE) {
			size *= 4
			if size > 1<<24 {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		var names []string
		for _, name := range bytes.Split(buf[:n], []byte{0}) {
			if len(name) > 0 {
				names = append(names, string(name))
			}
		}
		return names, nil
	}
}

// getXattr reads one attribute of fd (when ≥ 0) or of path itself.
func getXattr(path string, fd int, name string) ([]byte, error) {
	size := 256
	for {
		buf := make([]byte, size)
		var n int
		var err error
		if fd >= 0 {
			n, err = unix.Fgetxattr(fd, name, buf)
		} else {
			n, err = unix.Lgetxattr(path, name, buf)
		}
		if errors.Is(err, unix.ERANGE) {
			size *= 4
			if size > 1<<24 {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

// setXattr sets one attribute of fd (when ≥ 0) or of path itself.
func setXattr(path string, fd int, name string, value []byte) error {
	if fd >= 0 {
		return unix.Fsetxattr(fd, name, value, 0)
	}
	return unix.Lsetxattr(path, name, value, 0)
}
