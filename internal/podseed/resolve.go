package podseed

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxLinks bounds the symlinks one resolution follows, as the kernel's 40.
const maxLinks = 40

// errNoRoot is a path whose top-level dir is neither in the new generation nor
// one of the image's top-level symlinks.
var errNoRoot = errors.New("not in the root generation")

// genRoot is a root generation on the PVC as the instance pod will see it:
// /<d> is root/<d> or root-extra/<d>, and the image's top-level symlinks
// (/bin -> usr/bin, /home -> var/home) stay in the image, so their targets are
// read from the image root itself.
type genRoot struct {
	// pvc is the PVC mount.
	pvc string
	// dirs and extra are the top-level dirs under root/ and root-extra/.
	dirs, extra map[string]bool
	// topLinks are the image's top-level symlinks and their targets.
	topLinks map[string]string
}

// hostPath is where the resolved, symlink-free path components live on the
// PVC; "" when the top-level dir is in neither root/ nor root-extra/.
func (g genRoot) hostPath(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	base := ""
	switch {
	case g.dirs[parts[0]]:
		base = filepath.Join(g.pvc, DirRoot)
	case g.extra[parts[0]]:
		base = filepath.Join(g.pvc, DirRootExtra)
	default:
		return ""
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

// resolve is where the absolute path p lives on the PVC, following every
// symlink the way the pod's kernel will (absolute targets from the root,
// relative ones from their dir, `..` stopping at /) but never leaving the
// generation. A missing final component is fine (the result is where it would
// be created); a missing parent is fs.ErrNotExist.
func (g genRoot) resolve(p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("resolve %q: not absolute", p)
	}
	rest := splitPath(p)
	var cur []string
	links := 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case ".":
			continue
		case "..":
			if len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
			continue
		}
		next := append(append([]string(nil), cur...), c)
		var target string
		if len(next) == 1 && g.topLinks[c] != "" {
			target = g.topLinks[c]
		} else {
			host := g.hostPath(next)
			if host == "" {
				return "", fmt.Errorf("resolve %s: /%s is %w", p, next[0], errNoRoot)
			}
			fi, err := os.Lstat(host)
			if errors.Is(err, fs.ErrNotExist) && len(rest) == 0 {
				return host, nil
			}
			if err != nil {
				return "", fmt.Errorf("resolve %s: %w", p, err)
			}
			if fi.Mode()&fs.ModeSymlink == 0 {
				if len(rest) > 0 && !fi.IsDir() {
					return "", fmt.Errorf("resolve %s: /%s is not a directory", p, strings.Join(next, "/"))
				}
				cur = next
				continue
			}
			if target, err = os.Readlink(host); err != nil {
				return "", fmt.Errorf("resolve %s: %w", p, err)
			}
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("resolve %s: too many levels of symbolic links", p)
		}
		if path.IsAbs(target) {
			cur = nil
		}
		rest = append(splitPath(target), rest...)
	}
	if len(cur) == 0 {
		return "", fmt.Errorf("resolve %s: the root itself is not on the PVC", p)
	}
	return g.hostPath(cur), nil
}

// resolveDir resolves p and requires it to be an existing directory.
func (g genRoot) resolveDir(p string) (string, error) {
	host, err := g.resolve(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(host)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("resolve %s: not a directory", p)
	}
	return host, nil
}

func splitPath(p string) []string {
	var parts []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			parts = append(parts, c)
		}
	}
	return parts
}
