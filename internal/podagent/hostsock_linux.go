//go:build linux

package podagent

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// hostsock_linux.go binds the devcontainer's daemon-host socket binds in the
// pod (a gpg agent's at ~/.gnupg/S.gpg-agent, an X11 display's in
// /tmp/.X11-unix): a socket at each target the DesiredState names, whose
// every connection is relayed to fleetd on a TagSockHost stream naming the
// target. fleetd dials the daemon-host path it recorded for that target at
// create; the pod never names one.
//
// Where docker would show the daemon host's own socket (its owner the host
// user, which a devcontainer's user usually shares the uid of), the pod's
// is the devcontainer's user's, with the daemon-host socket's permission
// bits, and so are the directories made for it (0700, as gnupg and ssh want
// theirs). The agent is root, but it never acts as root where a pod user
// could swap a directory under it: in a directory only root can change (and
// that directory's every ancestor) it binds as root and then gives the
// socket to the user; anywhere else it binds as the user (asUser), whose
// permissions the kernel then checks, as if the user had bound it.

// hostDirMode is the mode of a directory made for a host-socket bind.
const hostDirMode = 0o700

// reservedHostDirs are where a host-socket bind cannot go (besides the
// control directory): fleet's own install, and the kernel's.
var reservedHostDirs = []string{"/opt/fleet", "/proc", "/sys", "/dev"}

// syncHostsLocked binds the host sockets of want and unbinds the others (and
// those whose mode changed). It returns what failed.
func (c *controlDir) syncHostsLocked(want []podwire.HostSocket) []string {
	keep := make(map[string]fs.FileMode, len(want))
	for _, h := range want {
		keep[h.Target] = h.Mode
	}
	for target, s := range c.hosts {
		if m, ok := keep[target]; !ok || m != s.mode {
			c.unbindHostLocked(target)
		}
	}
	for target := range c.failed {
		if t, ok := strings.CutPrefix(target, "host socket "); ok {
			if _, wanted := keep[t]; !wanted {
				delete(c.failed, target)
			}
		}
	}
	var problems []string
	for _, h := range want {
		if err := c.ensureHostLocked(h); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

// ensureHostLocked binds the host socket h unless it is bound and its file is
// still its own.
func (c *controlDir) ensureHostLocked(h podwire.HostSocket) error {
	if s := c.hosts[h.Target]; s != nil {
		if id, ok := socketID(s.path); ok && id == s.id {
			return nil
		}
		c.log.Warn("a host socket's file was removed or replaced; binding it again", "socket", s.path)
		c.unbindHostLocked(h.Target)
	}
	err := c.bindHostLocked(h)
	c.reportLocked("host socket "+h.Target, err)
	return err
}

func (c *controlDir) bindHostLocked(h podwire.HostSocket) error {
	if err := c.checkHostTarget(h.Target); err != nil {
		return err
	}
	id, switchUser, err := c.owner()
	if err != nil {
		return fmt.Errorf("bind the host socket %s: whom it belongs to: %w", h.Target, err)
	}
	ln, bound, asOwner, err := bindHostSocket(h.Target, h.Mode, id, switchUser)
	if err != nil {
		return fmt.Errorf("bind the host socket %s: %w", h.Target, err)
	}
	fid, ok := socketID(h.Target)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("bind the host socket %s: it is gone once bound", h.Target)
	}
	s := &inPodSocket{tag: podwire.TagSockHost, path: h.Target, ln: ln, id: fid, done: make(chan struct{}),
		target: h.Target, mode: h.Mode, bound: bound}
	if asOwner {
		s.owner = &id
	}
	c.hosts[h.Target] = s
	go c.accept(s)
	return nil
}

// checkHostTarget refuses a target in the control directory or a reserved
// one: the translator refuses binds there, so a state naming one is not
// fleetd's.
func (c *controlDir) checkHostTarget(target string) error {
	if !podwire.ValidHostSocketTarget(target) {
		return fmt.Errorf("bind the host socket %q: not a clean absolute socket path", target)
	}
	for _, dir := range append([]string{c.dir}, reservedHostDirs...) {
		if target == dir || strings.HasPrefix(target, strings.TrimSuffix(dir, "/")+"/") {
			return fmt.Errorf("bind the host socket %s: %s is fleet's or the kernel's", target, dir)
		}
	}
	return nil
}

// unbindHostLocked stops serving the host socket at target and removes its
// file (as whoever bound it), unless the file is no longer its own.
func (c *controlDir) unbindHostLocked(target string) {
	s := c.hosts[target]
	if s == nil {
		return
	}
	delete(c.hosts, target)
	_ = s.ln.Close()
	<-s.done
	remove := func() error {
		if id, ok := socketID(s.bound); ok && id == s.id {
			return os.Remove(s.bound)
		}
		return nil
	}
	if s.owner != nil {
		_ = asUser(*s.owner, true, remove)
	} else {
		_ = remove()
	}
}

// bindHostSocket binds a socket at target with perm, owned by id: the
// directories missing on the way made as id's (0700), and the socket bound
// as root in a directory only root can change (bound is then target in the
// directory's resolved path) or as id (asOwner) anywhere else. Without
// switchUser (single-uid mode, or an id that is root) everything is the
// agent's own.
func bindHostSocket(target string, perm fs.FileMode, id execas.Identity, switchUser bool) (ln *net.UnixListener, bound string, asOwner bool, err error) {
	if perm == 0 {
		perm = 0o600
	}
	dir := filepath.Dir(target)
	if !switchUser || (id.UID == 0 && id.GID == 0) {
		if err := os.MkdirAll(dir, hostDirMode); err != nil {
			return nil, "", false, err
		}
		ln, err := listenSocket(target, perm)
		return ln, target, false, err
	}
	if err := makeOwnedDirs(dir, id); err != nil {
		return nil, "", false, err
	}
	if real, ok := rootOnlyDir(dir); ok {
		path := filepath.Join(real, filepath.Base(target))
		ln, err := listenSocket(path, perm)
		if err != nil {
			return nil, "", false, err
		}
		if err := os.Lchown(path, int(id.UID), int(id.GID)); err != nil {
			_ = ln.Close()
			_ = os.Remove(path)
			return nil, "", false, err
		}
		return ln, path, false, nil
	}
	err = asUser(id, true, func() error {
		var lerr error
		ln, lerr = listenSocket(target, perm)
		return lerr
	})
	return ln, target, true, err
}

// listenSocket binds a unix socket at path, replacing whatever non-directory
// is there (a stale socket of an earlier session on the persisted root, or
// the image's placeholder, which docker's bind would hide), and gives it
// perm.
func listenSocket(path string, perm fs.FileMode) (*net.UnixListener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.IsDir() {
			return nil, fmt.Errorf("%s is a directory", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove what is at %s: %w", path, err)
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Unlinked by unbindHostLocked, and only while the file is still this
	// socket's.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, perm); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// makeOwnedDirs makes the directories missing on the way to dir, each id's
// and 0700: as root (then given to id) inside a directory only root can
// change, as id anywhere else.
func makeOwnedDirs(dir string, id execas.Identity) error {
	var missing []string
	existing := dir
	for {
		fi, err := os.Stat(existing)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", existing)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, filepath.Base(existing))
		existing = filepath.Dir(existing)
	}
	parent := existing
	for i := len(missing) - 1; i >= 0; i-- {
		next := filepath.Join(parent, missing[i])
		if real, ok := rootOnlyDir(parent); ok {
			made := filepath.Join(real, missing[i])
			if err := os.Mkdir(made, hostDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := os.Lchown(made, int(id.UID), int(id.GID)); err != nil {
				return err
			}
		} else if err := asUser(id, true, func() error {
			if err := os.Mkdir(next, hostDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		parent = next
	}
	return nil
}

// rootOnlyDir reports whether only root can change what dir is: every entry
// on the way to it (each link too, wherever it leads) is in a directory
// only root can change, one that is root's and not writable by its group or
// others, or sticky with the entry root's (as /tmp is, for root's own
// entries), and so is dir itself. real is dir with its links resolved: what
// to act on, so no link is followed again. A path through a link a user
// could have planted is not root's, wherever it leads.
func rootOnlyDir(dir string) (real string, ok bool) {
	cur := "/"
	curFi, err := os.Lstat(cur)
	if err != nil || !rootsOnly(curFi) {
		return "", false
	}
	rest := strings.Split(strings.Trim(dir, "/"), "/")
	for hops := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			if curFi, err = os.Lstat(cur); err != nil {
				return "", false
			}
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil || !protectedEntry(curFi, fi) {
			return "", false
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if hops++; hops > maxLinkHops {
				return "", false
			}
			link, err := os.Readlink(next)
			if err != nil {
				return "", false
			}
			if filepath.IsAbs(link) {
				cur = "/"
				if curFi, err = os.Lstat(cur); err != nil {
					return "", false
				}
			}
			rest = append(strings.Split(link, "/"), rest...)
			continue
		}
		if !fi.IsDir() {
			return "", false
		}
		cur, curFi = next, fi
	}
	if ownerOf(curFi) != 0 || (curFi.Mode().Perm()&0o022 != 0 && curFi.Mode()&fs.ModeSticky == 0) {
		return "", false
	}
	return cur, true
}

// maxLinkHops bounds the links rootOnlyDir follows (the kernel's own limit).
const maxLinkHops = 40

// protectedEntry reports whether only root can replace entry in dir: dir is
// root's, and not writable by its group or others, or sticky with the entry
// root's.
func protectedEntry(dir, entry fs.FileInfo) bool {
	if ownerOf(dir) != 0 {
		return false
	}
	if dir.Mode().Perm()&0o022 == 0 {
		return true
	}
	return dir.Mode()&fs.ModeSticky != 0 && ownerOf(entry) == 0
}

// rootsOnly reports whether a directory is root's and only root can write
// it.
func rootsOnly(fi fs.FileInfo) bool {
	return fi.IsDir() && ownerOf(fi) == 0 && fi.Mode().Perm()&0o022 == 0
}

func ownerOf(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
