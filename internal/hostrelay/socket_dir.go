package hostrelay

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SocketDir is a daemon-host directory of sockets a devcontainer binds (an
// X11 display's /tmp/.X11-unix, a gpg agent's socket directory), as fleetd
// records it at create. Docker binds the directory, so a socket made in it
// later shows in the container too: fleetd lists Source while the pod has a
// session, the pod's agent binds a socket at Target/<name> for each socket it
// finds there, and each connection to one is relayed to Source/<name>.
type SocketDir struct {
	// Target is the directory's path in the dev container.
	Target string `json:"target"`
	// Source is the directory's path on the daemon host, resolved (Resolve)
	// when it was recorded.
	Source string `json:"source"`
	// Shadowed are names in it another bind takes (a socket bound at
	// Target/<name>, as the deeper mount is on docker): never listed.
	Shadowed []string `json:"shadowed,omitempty"`
}

// Lists reports whether d relays name: not one another bind shadows.
func (d SocketDir) Lists(name string) bool {
	for _, s := range d.Shadowed {
		if s == name {
			return false
		}
	}
	return true
}

// DirSocket is one socket ListSockets found: its name in the directory and
// its permission bits.
type DirSocket struct {
	Name string
	Mode fs.FileMode
}

// ListSockets lists the sockets directly in dir, a recorded Source, by name:
// dir itself must be a directory, not a symbolic link (a link put at a
// recorded path is someone's redirect, as Dial refuses it), and only sockets
// are listed (no link to one).
func ListSockets(dir string) ([]DirSocket, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, &fs.PathError{Op: "list", Path: dir, Err: fmt.Errorf("not a clean absolute path")}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, &fs.PathError{Op: "list", Path: dir, Err: fmt.Errorf("%w: %s (resolved when the directory was recorded; fleet does not follow a link that appeared since)", ErrLink, dir)}
	case !fi.IsDir():
		return nil, &fs.PathError{Op: "list", Path: dir, Err: fmt.Errorf("not a directory")}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []DirSocket
	for _, e := range entries {
		if e.Type()&fs.ModeSocket == 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // gone since the listing
		}
		out = append(out, DirSocket{Name: e.Name(), Mode: info.Mode().Perm()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// InDir reports whether target is a socket of one of dirs, and the
// daemon-host path a connection to it is relayed to: Source/<name> of the
// first directory whose Target is target's parent, for a plain name the
// directory lists.
func InDir(dirs []SocketDir, target string) (source string, ok bool) {
	if target == "" || path.Clean(target) != target || !path.IsAbs(target) {
		return "", false
	}
	parent, name := path.Split(target)
	parent = strings.TrimSuffix(parent, "/")
	if name == "" || name == "." || name == ".." || parent == "" {
		return "", false
	}
	for _, d := range dirs {
		if d.Target == parent && d.Source != "" && d.Lists(name) {
			return filepath.Join(d.Source, name), true
		}
	}
	return "", false
}
