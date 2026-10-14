package podagent

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// A bundle is a gzip tar fleetd stages into a pod's /opt/fleet as a whole
// tree (the static tmux and its terminfo), content-addressed like the fleet
// binary: unpacked into <dir>/<name>-<sha256>, then <dir>/<name> is pointed
// at it with one rename, so a reader sees the old tree or the new one, never
// half of either. The previous tree is kept (a running tmux server may still
// use it); older ones are deleted.

// Caps on one bundle.
const (
	maxBundleBytes   = 256 << 20
	maxBundleEntries = 10000
)

// BundleOptions name one bundle install.
type BundleOptions struct {
	// Dir is where it is installed (/opt/fleet), absolute.
	Dir string
	// Name is the link's name (tmux): lowercase letters, digits and dashes.
	Name string
	// SHA256 is the gzip tar's hex sha256.
	SHA256 string
}

func (o BundleOptions) validate() error {
	if !filepath.IsAbs(o.Dir) {
		return fmt.Errorf("bundle dir %q is not absolute", o.Dir)
	}
	if o.Name == "" || strings.Trim(o.Name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return fmt.Errorf("bad bundle name %q", o.Name)
	}
	if !isHex64(o.SHA256) {
		return fmt.Errorf("bundle sha256 %q is not 64 lowercase hex digits", o.SHA256)
	}
	return nil
}

func (o BundleOptions) tree() string { return o.Name + "-" + o.SHA256 }

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// BundleInstalled reports whether the bundle o names is the installed one.
func BundleInstalled(o BundleOptions) bool {
	if o.validate() != nil {
		return false
	}
	target, err := os.Readlink(filepath.Join(o.Dir, o.Name))
	if err != nil || target != o.tree() {
		return false
	}
	fi, err := os.Stat(filepath.Join(o.Dir, o.tree()))
	return err == nil && fi.IsDir()
}

// InstallBundle installs the gzip tar read from r as the bundle o names. It
// reads r to its end and checks the sha256 before the tree goes live; an
// already installed bundle is left as it is, without reading r.
func InstallBundle(r io.Reader, o BundleOptions) error {
	if err := o.validate(); err != nil {
		return err
	}
	if BundleInstalled(o) {
		return nil
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(o.Dir, "."+o.tree()+".tmp-"+strconv.Itoa(os.Getpid()))
	_ = os.RemoveAll(tmp)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	h := sha256.New()
	if err := unpackBundle(io.TeeReader(r, h), tmp); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != o.SHA256 {
		return fmt.Errorf("bundle %s: sha256 %s, want %s", o.Name, got, o.SHA256)
	}
	final := filepath.Join(o.Dir, o.tree())
	if fi, err := os.Lstat(final); err == nil && fi.IsDir() {
		// Unpacked by an earlier install that did not get to the link.
	} else if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return linkBundle(o)
}

// unpackBundle unpacks a gzip tar into dir, refusing anything that would
// land outside it: absolute or escaping names, symlinks out of the tree,
// devices, and hard links to anything but a file already unpacked.
func unpackBundle(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("bundle is not gzip: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(zr)
	var total int64
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("bundle: %w", err)
		}
		if entries >= maxBundleEntries {
			return fmt.Errorf("bundle has more than %d entries", maxBundleEntries)
		}
		name, err := bundlePath(hdr.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		perm := fs.FileMode(hdr.Mode) & fs.ModePerm
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			if err := root.Chmod(name, perm|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxBundleBytes {
				return fmt.Errorf("bundle unpacks to more than %d bytes", maxBundleBytes)
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				_ = f.Close()
				return fmt.Errorf("bundle %s: %w", hdr.Name, err)
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := root.Chmod(name, perm); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := bundlePath(hdr.Linkname)
			if err != nil {
				return err
			}
			if fi, err := root.Lstat(target); err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("bundle %s: hard link to %q, which is not a file in the bundle", hdr.Name, hdr.Linkname)
			}
			if err := root.Link(target, name); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(hdr.Linkname) {
				return fmt.Errorf("bundle %s: symlink to an absolute path", hdr.Name)
			}
			if _, err := bundlePath(path.Join(path.Dir(name), hdr.Linkname)); err != nil {
				return fmt.Errorf("bundle %s: symlink out of the bundle", hdr.Name)
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("bundle %s: unsupported entry type %q", hdr.Name, hdr.Typeflag)
		}
	}
	// The sha256 covers the whole stream: read what follows the archive.
	_, err = io.Copy(io.Discard, r)
	return err
}

// bundlePath is an entry name made relative and clean, or an error when it
// leaves the bundle.
func bundlePath(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", fmt.Errorf("bundle entry %q is outside the bundle", name)
	}
	return clean, nil
}

// linkBundle points <dir>/<name> at the bundle's tree, then deletes the trees
// older than the one it replaced.
func linkBundle(o BundleOptions) error {
	link := filepath.Join(o.Dir, o.Name)
	prev, _ := os.Readlink(link)
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		// Only fleet writes here: a tree in the link's place is an older
		// layout's, and the link replaces it.
		if err := os.RemoveAll(link); err != nil {
			return err
		}
	}
	next := filepath.Join(o.Dir, "."+o.Name+".new")
	_ = os.Remove(next)
	if err := os.Symlink(o.tree(), next); err != nil {
		return err
	}
	if err := os.Rename(next, link); err != nil {
		_ = os.Remove(next)
		return err
	}
	entries, err := os.ReadDir(o.Dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, o.Name+"-") || n == o.tree() || n == prev {
			continue
		}
		if isHex64(strings.TrimPrefix(n, o.Name+"-")) {
			_ = os.RemoveAll(filepath.Join(o.Dir, n))
		}
	}
	return nil
}
