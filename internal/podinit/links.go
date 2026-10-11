package podinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// makeLinks makes the spec's links: each Path a symlink to its Target, its
// parent dirs created (0755) where missing, and owned as the target is: what
// stat (not following the link) says of the bound socket is then what the
// daemon host's would say, so the docker-outside-of-docker feature joins the
// socket's group as it does on a Linux docker host, rather than proxying it
// through socat as for a root-group socket. Each Path is where the run bound
// a socket, whose mount would hide what the image has there on docker, so a
// file at Path (the image's placeholder, such as the empty
// /var/run/docker-host.sock the docker-outside-of-docker feature leaves, or
// a stale link or socket of an earlier run on the persisted root) is
// replaced. A directory there stays, with a warning: docker refuses a socket
// bind over one too. It returns the warnings.
func makeLinks(links []Link) []string {
	var warns []string
	for _, l := range links {
		if cur, err := os.Readlink(l.Path); err == nil && cur == l.Target {
			continue
		}
		fi, err := os.Lstat(l.Path)
		switch {
		case err == nil && fi.IsDir():
			warns = append(warns, fmt.Sprintf("link %s to %s: %s is already there; left as it is", l.Path, l.Target, describeMode(fi.Mode())))
			continue
		case err == nil:
			if err := os.Remove(l.Path); err != nil {
				warns = append(warns, fmt.Sprintf("link %s to %s: %v", l.Path, l.Target, err))
				continue
			}
		case !errors.Is(err, fs.ErrNotExist):
			warns = append(warns, fmt.Sprintf("link %s to %s: %v", l.Path, l.Target, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
			warns = append(warns, fmt.Sprintf("link %s to %s: %v", l.Path, l.Target, err))
			continue
		}
		if err := os.Symlink(l.Target, l.Path); err != nil {
			warns = append(warns, fmt.Sprintf("link %s to %s: %v", l.Path, l.Target, err))
			continue
		}
		ownLikeTarget(l.Path, l.Target)
	}
	return warns
}

func describeMode(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m.IsRegular():
		return "a file"
	}
	return "a " + m.Type().String() + " file"
}
