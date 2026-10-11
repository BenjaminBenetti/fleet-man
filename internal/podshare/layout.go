package podshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
)

// LayoutVersion is the layout format this fleet writes and reads.
const LayoutVersion = 1

// maxLayout bounds a layout file.
const maxLayout = 1 << 20

// Entry is one path of the fleet's folders (a resolver Layout entry): a
// folder a mount is made of, the shared files folder, or a file in it.
type Entry struct {
	// Path is slash-separated and relative to the fleet's mount root
	// (".claude", "files/.claude.json").
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
	// Mode is the permission bits the path gets.
	Mode uint32 `json:"mode"`
	// Seed is what a missing file is created with; a file without one is
	// left missing (the kubelet never mounts a file of the tree by itself,
	// and an empty one would read as content of its own: a JSON settings
	// file that parses as nothing waits forever for a write that makes it
	// whole).
	Seed string `json:"seed,omitempty"`
}

// Layout is what the share-layout init container makes of the tree before
// the dev container mounts its folders out of it.
type Layout struct {
	V       int     `json:"v"`
	Entries []Entry `json:"entries"`
	// UID and GID own the fleet's folders and the spoke's state: the share
	// uid, which the pod's users and the spoke run as. -1 leaves ownership
	// to whoever creates them (a pod without root).
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// Validate checks l is a layout this fleet can replay safely.
func (l Layout) Validate() error {
	if l.V > LayoutVersion {
		return fmt.Errorf("layout version %d is newer than this fleet's %d", l.V, LayoutVersion)
	}
	for _, e := range l.Entries {
		if !validPath(e.Path) {
			return fmt.Errorf("layout path %q is not a clean path below the fleet's mount root", e.Path)
		}
		if e.Mode&^0o7777 != 0 || e.Mode&0o6000 != 0 {
			return fmt.Errorf("layout path %s: mode %o is not plain permission bits", e.Path, e.Mode)
		}
		if e.Dir && e.Seed != "" {
			return fmt.Errorf("layout path %s: a folder has no seed", e.Path)
		}
	}
	if (l.UID < 0) != (l.GID < 0) {
		return fmt.Errorf("layout owner %d:%d sets one id without the other", l.UID, l.GID)
	}
	return nil
}

// validPath reports a clean, relative, slash-separated path with no "..".
func validPath(p string) bool {
	return p != "" && p != "." && fs.ValidPath(p) && !strings.Contains(p, "\\")
}

// LoadLayout reads a layout file.
func LoadLayout(name string) (Layout, error) {
	f, err := os.Open(name)
	if err != nil {
		return Layout{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLayout+1))
	if err != nil {
		return Layout{}, err
	}
	if len(data) > maxLayout {
		return Layout{}, fmt.Errorf("layout %s is over %d bytes", name, maxLayout)
	}
	var l Layout
	if err := json.Unmarshal(data, &l); err != nil {
		return Layout{}, fmt.Errorf("layout %s: %w", name, err)
	}
	return l, l.Validate()
}

// The tree's own paths' modes.
const (
	// treeMode is the tree's root and RootsDir: root's, so no pod user
	// renames a fleet folder away (or swaps a link in for it) between this
	// replay and the kubelet mounting it.
	treeMode fs.FileMode = 0o755
	// parentMode is a missing folder above a layout path, as mkdir -p makes
	// one.
	parentMode fs.FileMode = 0o755
	stateMode  fs.FileMode = 0o700
	leaseMode  fs.FileMode = 0o600
)

// Apply makes l's tree in dir (MountDir: the share's root, or the copy
// tier's DiskDir, as the pod mounts it) without following a symlink anywhere
// in it: RootsDir with every layout path, StateDir and LeaseFile.
//
// A symlink where a folder of the tree belongs, at or above a layout path
// (something in another of the fleet's pods made one), is removed and the
// folder made: the kubelet follows a link inside the volume when it mounts a
// folder, so a link left there would mount another part of the tree (the
// spoke's state, say) into the pod. A missing file gets its seed (none: left
// missing); an existing one keeps its content. Each layout path, its
// missing parents, the state and the lease get the layout's owner; the
// tree's root and RootsDir are root's (when running as root). Something else
// than a folder where one belongs fails the replay, naming it: the host's
// resolver refuses the same tree.
func Apply(dir string, l Layout) error {
	if err := l.Validate(); err != nil {
		return err
	}
	r, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		return fmt.Errorf("the fleet's share at %s: %w", dir, err)
	}
	defer r.Close()
	root := os.Geteuid() == 0
	treeUID, treeGID := -1, -1
	if root {
		treeUID, treeGID = 0, 0
	}
	a := applier{r: r, top: dir}
	if err := a.own(".", treeMode, treeUID, treeGID); err != nil {
		return err
	}
	if err := a.dir(RootsDir, treeMode, treeUID, treeGID); err != nil {
		return err
	}
	if err := a.dir(StateDir, stateMode, l.UID, l.GID); err != nil {
		return err
	}
	if err := a.file(LeaseFile, leaseMode, l.UID, l.GID, "", true); err != nil {
		return err
	}
	for _, e := range l.Entries {
		name := path.Join(RootsDir, e.Path)
		if err := a.parents(name, l.UID, l.GID); err != nil {
			return err
		}
		mode := fs.FileMode(e.Mode)
		if e.Dir {
			err = a.dir(name, mode, l.UID, l.GID)
		} else {
			err = a.file(name, mode, l.UID, l.GID, e.Seed, false)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// applier replays one layout.
type applier struct {
	r   *safefs.Root
	top string
}

func (a applier) host(name string) string { return path.Join(a.top, name) }

// parents makes the folders above name below RootsDir that are missing (or
// symlinks); one that exists keeps its mode and owner (it may be a layout
// path of its own, the shared files folder above its files).
func (a applier) parents(name string, uid, gid int) error {
	parts := strings.Split(name, "/")
	for i := 2; i < len(parts); i++ {
		p := strings.Join(parts[:i], "/")
		if info, err := a.r.Lstat(p); err == nil && info.IsDir() {
			continue
		}
		if err := a.dir(p, parentMode, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// dir makes name a folder (replacing a symlink there) with mode, owned by
// uid:gid (-1 keeps the owner).
func (a applier) dir(name string, mode fs.FileMode, uid, gid int) error {
	info, err := a.r.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := a.r.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("making %s: %w", a.host(name), err)
		}
	case err != nil:
		return fmt.Errorf("%s: %w", a.host(name), err)
	case info.Mode()&fs.ModeSymlink != 0:
		if err := a.r.Remove(name); err != nil {
			return fmt.Errorf("removing the symlink at %s: %w", a.host(name), err)
		}
		if err := a.r.Mkdir(name, 0o700); err != nil {
			return fmt.Errorf("making %s: %w", a.host(name), err)
		}
	case !info.IsDir():
		return fmt.Errorf("%s is a %s where the fleet's folder belongs; remove it from the share", a.host(name), kind(info))
	}
	return a.own(name, mode, uid, gid)
}

// file makes name a regular file: a symlink there goes, and a missing file
// is created with seed (empty when always), else left missing.
func (a applier) file(name string, mode fs.FileMode, uid, gid int, seed string, always bool) error {
	info, err := a.r.Lstat(name)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		if err := a.r.Remove(name); err != nil {
			return fmt.Errorf("removing the symlink at %s: %w", a.host(name), err)
		}
		err = fs.ErrNotExist
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("%s is a %s where the fleet's file belongs; remove it from the share", a.host(name), kind(info))
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if seed == "" && !always {
			return nil
		}
		f, err := a.r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			break // another pod's replay made it meanwhile
		}
		if err != nil {
			return fmt.Errorf("making %s: %w", a.host(name), err)
		}
		_, werr := io.WriteString(f, seed)
		cerr := f.Close()
		if err := errors.Join(werr, cerr); err != nil {
			return fmt.Errorf("writing %s: %w", a.host(name), err)
		}
	case err != nil:
		return fmt.Errorf("%s: %w", a.host(name), err)
	}
	return a.own(name, mode, uid, gid)
}

// own gives name its owner (unless -1) and mode.
func (a applier) own(name string, mode fs.FileMode, uid, gid int) error {
	if uid >= 0 {
		if err := a.r.Lchown(name, uid, gid); err != nil {
			return fmt.Errorf("giving %s to %d:%d: %w", a.host(name), uid, gid, err)
		}
	}
	if err := a.r.Chmod(name, mode); err != nil {
		return fmt.Errorf("setting %s's mode: %w", a.host(name), err)
	}
	return nil
}

func kind(info fs.FileInfo) string {
	switch m := info.Mode(); {
	case m.IsRegular():
		return "file"
	case m&fs.ModeNamedPipe != 0:
		return "FIFO"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

// PrivateSyncDir makes SyncDir (dir) root's alone when running as root, so
// the pod's users never reach the tree mounted inside it but through their
// own folders. Without root nothing can be made private, and nothing is.
func PrivateSyncDir(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the share's mount folder %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the share's mount folder %s is not a folder", dir)
	}
	if err := os.Lchown(dir, 0, 0); err != nil {
		return fmt.Errorf("giving %s to root: %w", dir, err)
	}
	return os.Chmod(dir, 0o700)
}
