//go:build linux

package podagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"golang.org/x/sys/unix"
)

// controldir_linux.go applies fleetd's DesiredState in the pod's control
// directory (Options.ControlDir, the pod's own emptyDir at
// /fleet-mounts/control): the in-pod sockets the main agent binds there, each
// connection to which it relays to fleetd on a stream of the socket's kind,
// and the files fleetd keeps there (the status mod's). Their names are kept in
// a manifest there too (filesManifestName), so the next agent (a reconnect, a
// takeover) removes the ones a later state leaves out, though it never wrote
// them itself.
//
// The main agent makes the directory root's and 0755 when it starts, so
// nothing in the pod but root can plant a file or a link in it. A socket is
// bound 0666: whether a connection is served is fleetd's call, per stream, as
// any process of a docker instance may connect to the daemon's sockets there.
// One whose file is no longer the one bound (removed, replaced) is bound
// again. The sockets live only as long as the session: when it ends they are
// unbound and their connections cut, as a docker instance's go when its daemon
// stops. The files stay; the status mod tells a stale one by its age.

// controlDir is the main agent's work in the control directory.
type controlDir struct {
	dir string
	log *Logger
	// dial opens a relay stream to fleetd for one in-pod connection.
	dial func(podwire.Tag, podwire.SockOpen) (net.Conn, error)

	mu     sync.Mutex
	closed bool
	want   podwire.Sockets
	socks  map[podwire.Tag]*inPodSocket
	files  map[string]writtenFile
	// loaded: the manifest of the files earlier agents wrote was read into
	// files (on the first state); manifest is what the manifest holds.
	loaded   bool
	manifest string
	// failed is what each socket or file last failed with: a failure is
	// logged once, until it changes or clears.
	failed map[string]string

	// connMu guards the relayed connections (never held with mu: an accept
	// loop takes it while unbinding waits for that loop under mu).
	connMu      sync.Mutex
	connsClosed bool
	conns       map[net.Conn]struct{}
	relays      sync.WaitGroup
}

// inPodSocket is one bound socket.
type inPodSocket struct {
	tag  podwire.Tag
	path string
	ln   *net.UnixListener
	id   fileID
	done chan struct{}
}

// fileID is what tells one file from another at a name.
type fileID struct {
	dev, ino uint64
}

// writtenFile is a control file as the agent last wrote it.
type writtenFile struct {
	data  []byte
	mode  fs.FileMode
	id    fileID
	size  int64
	mtime time.Time
}

func newControlDir(dir string, log *Logger, dial func(podwire.Tag, podwire.SockOpen) (net.Conn, error)) *controlDir {
	return &controlDir{
		dir: dir, log: log, dial: dial,
		socks:  map[podwire.Tag]*inPodSocket{},
		files:  map[string]writtenFile{},
		conns:  map[net.Conn]struct{}{},
		failed: map[string]string{},
	}
}

// prepareControlDir makes dir the pod's control directory: it exists and,
// when the agent is root, is root's and 0755. An agent that is not root (a
// pod in single-uid mode) shares its uid with every process in the pod, so
// there is nobody to keep out.
func prepareControlDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Lchown(dir, 0, 0); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// apply makes the directory match ds: the sockets it wants bound, the rest
// unbound, its files written and those of earlier states it leaves out
// removed. It returns what failed ("" when nothing did); each failure is
// also logged, once.
func (c *controlDir) apply(ds podwire.DesiredState) string {
	if err := ds.Validate(); err != nil {
		c.report("state", err)
		return err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "the session ended"
	}
	c.want = ds.Sockets
	if !c.loaded {
		c.loaded = true
		c.loadManifestLocked()
	}
	var problems []string
	for _, tag := range podwire.SockTags {
		if ds.Sockets.Wants(tag) {
			if err := c.ensureLocked(tag); err != nil {
				problems = append(problems, err.Error())
			}
		} else {
			c.unbindLocked(tag)
		}
	}
	keep := make(map[string]bool, len(ds.Files))
	for _, f := range ds.Files {
		keep[f.Name] = true
		err := c.writeLocked(f)
		c.reportLocked("file "+f.Name, err)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	for name := range c.files {
		if keep[name] {
			continue
		}
		err := os.Remove(filepath.Join(c.dir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.reportLocked("file "+name, fmt.Errorf("remove control file %s: %w", name, err))
			problems = append(problems, err.Error())
			continue
		}
		c.reportLocked("file "+name, nil)
		delete(c.files, name)
	}
	c.reportLocked("file "+filesManifestName, c.saveManifestLocked())
	return strings.Join(problems, "; ")
}

// maxFilesManifest bounds the manifest read back: it names at most
// podwire.MaxControlFiles plain base names.
const maxFilesManifest = 16 << 10

// loadManifestLocked takes the files the manifest says earlier agents wrote
// as this agent's: a state that leaves one out removes it, one that keeps it
// writes it anew.
func (c *controlDir) loadManifestLocked() {
	f, err := os.OpenFile(filepath.Join(c.dir, filesManifestName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFilesManifest))
	if err != nil {
		return
	}
	var names []string
	if json.Unmarshal(data, &names) != nil {
		return
	}
	c.manifest = string(data)
	for _, name := range names {
		if _, ok := c.files[name]; !ok && podwire.ValidControlFileName(name) {
			c.files[name] = writtenFile{}
		}
	}
}

// saveManifestLocked records the names of the files written for fleetd, when
// they changed.
func (c *controlDir) saveManifestLocked() error {
	names := slices.Sorted(maps.Keys(c.files))
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if string(data) == c.manifest {
		return nil
	}
	if len(names) == 0 {
		err = os.Remove(filepath.Join(c.dir, filesManifestName))
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	} else {
		err = replaceFile(c.dir, filesManifestName, data, 0o600)
	}
	if err != nil {
		return fmt.Errorf("record the control files: %w", err)
	}
	c.manifest = string(data)
	return nil
}

// check binds again every wanted socket whose file is no longer its own.
func (c *controlDir) check() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for _, tag := range podwire.SockTags {
		if c.want.Wants(tag) {
			_ = c.ensureLocked(tag)
		}
	}
}

// run checks the sockets every interval until done closes.
func (c *controlDir) run(done <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			c.check()
		}
	}
}

// relayCloseWait bounds close's wait for the relays it cut: one still waiting
// for fleetd's answer to its stream ends on its own.
const relayCloseWait = time.Second

// close unbinds every socket and cuts the connections relayed through them;
// apply does nothing afterwards. It returns once no socket it bound is left
// (and the relays ended, or relayCloseWait passed).
func (c *controlDir) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, tag := range podwire.SockTags {
		c.unbindLocked(tag)
	}
	c.mu.Unlock()
	c.connMu.Lock()
	c.connsClosed = true
	for conn := range c.conns {
		_ = conn.Close()
	}
	c.connMu.Unlock()
	ended := make(chan struct{})
	go func() {
		c.relays.Wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(relayCloseWait):
	}
}

// ensureLocked binds the socket of kind tag unless it is bound and its file
// is still its own.
func (c *controlDir) ensureLocked(tag podwire.Tag) error {
	if s := c.socks[tag]; s != nil {
		if id, ok := socketID(s.path); ok && id == s.id {
			return nil
		}
		c.log.Warn("an in-pod socket's file was removed or replaced; binding it again", "socket", s.path)
		c.unbindLocked(tag)
	}
	path := filepath.Join(c.dir, podwire.SockName(tag))
	err := c.bindLocked(tag, path)
	c.reportLocked("socket "+path, err)
	return err
}

func (c *controlDir) bindLocked(tag podwire.Tag, path string) error {
	// Whatever is at the name goes: a socket an agent before this one left,
	// or anything planted before the directory was made root's. Remove
	// unlinks a link itself, never what it points at.
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("bind the in-pod socket %s: remove what is there: %w", path, err)
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("bind the in-pod socket %s: %w", path, err)
	}
	// Unlinked here, and only while the file is still this socket's.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o666); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return fmt.Errorf("bind the in-pod socket %s: %w", path, err)
	}
	id, ok := socketID(path)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("bind the in-pod socket %s: it is gone once bound", path)
	}
	s := &inPodSocket{tag: tag, path: path, ln: ln, id: id, done: make(chan struct{})}
	c.socks[tag] = s
	go c.accept(s)
	return nil
}

// unbindLocked stops serving the socket of kind tag and removes its file,
// unless the file is no longer its own.
func (c *controlDir) unbindLocked(tag podwire.Tag) {
	s := c.socks[tag]
	if s == nil {
		return
	}
	delete(c.socks, tag)
	_ = s.ln.Close()
	<-s.done
	if id, ok := socketID(s.path); ok && id == s.id {
		_ = os.Remove(s.path)
	}
}

// accept relays every connection to s until it is closed.
func (c *controlDir) accept(s *inPodSocket) {
	defer close(s.done)
	for {
		conn, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient error (EMFILE): back off instead of spinning.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !c.track(conn) {
			_ = conn.Close()
			continue
		}
		go c.relay(s.tag, conn)
	}
}

func (c *controlDir) track(conn net.Conn) bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.connsClosed {
		return false
	}
	c.conns[conn] = struct{}{}
	c.relays.Add(1)
	return true
}

func (c *controlDir) untrack(conn net.Conn) {
	c.connMu.Lock()
	delete(c.conns, conn)
	c.connMu.Unlock()
	c.relays.Done()
}

// relay carries one in-pod connection to fleetd: the end of either side's
// input is a half-close of that direction on the other.
func (c *controlDir) relay(tag podwire.Tag, conn *net.UnixConn) {
	defer c.untrack(conn)
	defer conn.Close()
	up, err := c.dial(tag, podwire.SockOpen{Peer: peerOf(conn)})
	if err != nil {
		if !podwire.IsSockRefused(err, podwire.RefusedGate) {
			// fleetd says why it refuses a gated socket in its own log.
			c.log.Warn("could not relay a connection to fleetd", "socket", podwire.SockName(tag), "err", err.Error())
		}
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() {
		if _, err := io.Copy(up, conn); err == nil {
			closeWrite(up)
		} else {
			_ = up.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		if _, err := io.Copy(conn, up); err == nil {
			_ = conn.CloseWrite()
		} else {
			_ = conn.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// peerOf is conn's peer as the kernel reports it; nil when it cannot say.
func peerOf(conn *net.UnixConn) *podwire.Peer {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return nil
	}
	return &podwire.Peer{PID: int(cred.Pid), UID: int(cred.Uid), GID: int(cred.Gid)}
}

// writeLocked writes f unless the file is still what the last write of the
// same content left (the pod may have removed or replaced it).
func (c *controlDir) writeLocked(f podwire.ControlFile) error {
	path := filepath.Join(c.dir, f.Name)
	if w, ok := c.files[f.Name]; ok && w.mode == f.Mode && bytes.Equal(w.data, f.Data) && w.still(path) {
		return nil
	}
	if err := replaceFile(c.dir, f.Name, f.Data, f.Mode.Perm()); err != nil {
		return fmt.Errorf("write control file %s: %w", f.Name, err)
	}
	w := writtenFile{data: bytes.Clone(f.Data), mode: f.Mode}
	if fi, err := os.Lstat(path); err == nil {
		w.id, w.size, w.mtime = idOf(fi), fi.Size(), fi.ModTime()
	}
	c.files[f.Name] = w
	return nil
}

// replaceFile makes dir/name hold data with perm, atomically: a temp file
// renamed over the name, which replaces whatever is there (a link itself,
// never what it points at).
func replaceFile(dir, name string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(perm)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// still reports whether the file at path is the one written.
func (w writtenFile) still(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular() && idOf(fi) == w.id && fi.Size() == w.size && fi.ModTime().Equal(w.mtime)
}

// socketID is the identity of the socket at path; false when there is none.
func socketID(path string) (fileID, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fs.ModeSocket == 0 {
		return fileID{}, false
	}
	return idOf(fi), true
}

func idOf(fi fs.FileInfo) fileID {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fileID{dev: uint64(st.Dev), ino: st.Ino}
	}
	return fileID{}
}

func (c *controlDir) report(what string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reportLocked(what, err)
}

// reportLocked logs what's failure as an error, once until it changes; a
// failure that cleared is forgotten.
func (c *controlDir) reportLocked(what string, err error) {
	if err == nil {
		delete(c.failed, what)
		return
	}
	if c.failed[what] == err.Error() {
		return
	}
	c.failed[what] = err.Error()
	c.log.Error("the control directory is not as fleetd wants it", "what", what, "err", err.Error())
}
