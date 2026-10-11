//go:build !linux

package hostrelay

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// dialNoFollow checks each component of path with lstat, then dials it:
// without Linux's O_PATH descriptors a link swapped in between the two is
// not caught, which needs write access to a directory on the way.
func dialNoFollow(ctx context.Context, path string) (net.Conn, error) {
	walked := "/"
	for _, c := range strings.Split(strings.Trim(filepath.Dir(path), "/"), "/") {
		if c == "" {
			continue
		}
		walked = filepath.Join(walked, c)
		fi, err := os.Lstat(walked)
		if err != nil {
			return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, linkError(path, walked)
		}
		if !fi.IsDir() {
			return nil, &fs.PathError{Op: "dial", Path: path, Err: fmt.Errorf("%s is not a directory", walked)}
		}
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, &fs.PathError{Op: "dial", Path: path, Err: err}
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return nil, notSocketError(path, fi.Mode())
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
