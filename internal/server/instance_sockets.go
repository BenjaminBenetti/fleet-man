package server

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// instance_sockets.go owns the unix sockets the daemon serves INTO instances.
// An instance's control directory is bind-mounted at /fleet-mounts/control, so
// a socket the daemon listens on there is reachable from inside the instance.
// Two features use this: the SSH-agent relay (sshagent_listen.go) and the
// in-instance fleet MCP server (mcp_instance.go). Each keeps one
// instanceSocketSet, which listens on one socket name in the control directory
// of every instance the feature selects.
//
// The control directory is writable from the instance and traversable by other
// host users, so a socket there is bound without resolving any path the
// instance controls (listenInstanceSocket) and every connection is checked
// (peerAllowed): the daemon's user, root, or a process of that instance's own
// container — never another host user. Linux only.

const (
	// maxSlowPeerChecks is how many cross-uid peer checks (which may run
	// docker) one socket runs at once; more are refused.
	maxSlowPeerChecks = 8
	// refusalLogInterval spaces "refused a connection" warnings for one socket.
	refusalLogInterval = time.Minute
	// instanceListenRetry spaces retries of an instance socket that failed to
	// listen (e.g. a path over the unix-socket length limit).
	instanceListenRetry = 30 * time.Second
)

// socketListener is one unix socket the daemon serves.
type socketListener struct {
	ln   net.Listener
	path string
	// label names the feature in log lines ("ssh agent", "fleet mcp").
	label string
	// inst identifies the instance whose container's processes may connect
	// besides the daemon's user and root (nil for a host-only socket).
	inst instanceIdentity
	// unlink removes the socket file on Close (nil: the listener's own
	// unlink-on-close does).
	unlink func()
	// bound reports whether the socket file is still the one this listener
	// bound (nil: not tracked).
	bound func() bool

	mu     sync.Mutex
	closed bool
	done   chan struct{}
	// Refusals are logged at most once a minute per socket, with a count.
	lastRefusalLog time.Time
	refusals       int
	// slowChecks bounds concurrent container checks for cross-uid peers.
	slowChecks chan struct{}
}

func (l *socketListener) refused() {
	l.mu.Lock()
	l.refusals++
	if time.Since(l.lastRefusalLog) < refusalLogInterval {
		l.mu.Unlock()
		return
	}
	count := l.refusals
	l.refusals = 0
	l.lastRefusalLog = time.Now()
	l.mu.Unlock()
	flog.Warn(l.label+" socket: refused connections from another user", "socket", l.path, "count", count)
}

func (l *socketListener) acceptLoop(serve func(net.Conn)) {
	defer close(l.done)
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient error (EMFILE): back off instead of spinning.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// The peer check may ask docker (cached): never on the accept loop,
		// where one slow answer would hold up every other connection.
		go func() {
			if !peerAllowed(conn, l.inst, l.slowChecks) {
				l.refused()
				_ = conn.Close()
				return
			}
			serve(conn)
		}()
	}
}

// Close stops listening and removes the socket file.
func (l *socketListener) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.mu.Unlock()
	_ = l.ln.Close() // for a host socket this also unlinks the path
	<-l.done
	if l.unlink != nil {
		l.unlink()
	}
}

// instanceTarget is where one instance's socket goes and what identifies its
// container: the recorded ID (known once `devcontainer up` has returned) and
// the workspace folder its container is labelled with.
type instanceTarget struct {
	dir, containerID, workspace string
}

// loadInstanceTarget reads an instance's target from state.json. The record
// may still be StatusCreating (provisioning), in which case its workspace
// folder is how the container is recognized before its ID is recorded.
func loadInstanceTarget(fleetName, instanceName string) instanceTarget {
	t := instanceTarget{dir: state.ControlDir(fleetName, instanceName)}
	if st, err := state.Load(); err == nil {
		if f := st.Fleets[fleetName]; f != nil {
			for _, inst := range f.Instances {
				if inst.Name == instanceName {
					t.containerID, t.workspace = inst.ContainerID, inst.WorkspaceDir
				}
			}
		}
	}
	return t
}

// instanceListener is a per-instance socket plus what identifies its
// container.
type instanceListener struct {
	*socketListener
	dir       string
	container atomic.Value // string
	workspace atomic.Value // string
}

func (il *instanceListener) containerID() string {
	id, _ := il.container.Load().(string)
	return id
}

func (il *instanceListener) workspaceDir() string {
	dir, _ := il.workspace.Load().(string)
	return dir
}

// stillBound reports whether the socket file is still the one this listener
// bound (an instance destroyed and re-created under the same name between
// reconciles, or a process in the instance deleting or replacing it, leaves
// this listener bound to nothing).
func (il *instanceListener) stillBound() bool {
	if il.bound != nil {
		return il.bound()
	}
	info, err := os.Lstat(il.path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// instanceSocketSet keeps a socket named name listening in the control
// directory of every instance its owner wants, keyed "<fleet>/<instance>".
type instanceSocketSet struct {
	name  string
	label string
	// serve returns the accept handler for an instance's socket.
	serve func(key string) func(net.Conn)

	mu        sync.Mutex
	listeners map[string]*instanceListener
	opening   map[string]chan struct{} // listens in flight; closed when each settles
	retryAt   map[string]time.Time     // failed listens, retried after
	closed    bool
}

func newInstanceSocketSet(name, label string, serve func(key string) func(net.Conn)) *instanceSocketSet {
	return &instanceSocketSet{
		name:      name,
		label:     label,
		serve:     serve,
		listeners: make(map[string]*instanceListener),
		opening:   make(map[string]chan struct{}),
		retryAt:   make(map[string]time.Time),
	}
}

// sync listens for every instance in want and stops listening for the rest,
// and for any whose socket file is no longer the one it bound.
func (s *instanceSocketSet) sync(want map[string]instanceTarget) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	var stale []*instanceListener
	for key, il := range s.listeners {
		t, ok := want[key]
		if !ok || t.dir != il.dir || !il.stillBound() {
			stale = append(stale, il)
			delete(s.listeners, key)
			continue
		}
		il.container.Store(t.containerID)
		il.workspace.Store(t.workspace)
	}
	for key := range s.retryAt {
		if _, ok := want[key]; !ok {
			delete(s.retryAt, key)
		}
	}
	s.mu.Unlock()

	for _, il := range stale {
		il.Close()
	}
	for key, t := range want {
		s.open(key, t, false)
	}
}

// ensure opens key's socket right away — waiting for an open of the same
// socket already in flight, and trying again at once if that one failed — and
// reports whether it is listening when it returns.
func (s *instanceSocketSet) ensure(key string, t instanceTarget) bool {
	s.open(key, t, true)
	return s.listening(key)
}

// listening reports whether key's socket is being served.
func (s *instanceSocketSet) listening(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.listeners[key]
	return ok
}

// open listens for key unless it already is (or recently failed and is not
// due a retry, unless now is set).
func (s *instanceSocketSet) open(key string, t instanceTarget, now bool) {
	if !instanceSocketsSupported {
		return
	}
	for {
		s.mu.Lock()
		_, listening := s.listeners[key]
		at, failed := s.retryAt[key]
		if s.closed || listening || (failed && !now && time.Now().Before(at)) {
			s.mu.Unlock()
			return
		}
		if inflight, busy := s.opening[key]; busy {
			s.mu.Unlock()
			if !now {
				return
			}
			// The caller needs the socket when this returns: wait for a
			// reconcile's open of the same instance, and try again at once if
			// that one failed.
			<-inflight
			continue
		}
		settled := make(chan struct{})
		s.opening[key] = settled
		s.mu.Unlock()

		s.listen(key, t, failed)

		s.mu.Lock()
		delete(s.opening, key)
		close(settled)
		s.mu.Unlock()
		return
	}
}

// listen opens one instance's socket for open, which holds its slot in
// s.opening.
func (s *instanceSocketSet) listen(key string, t instanceTarget, failedBefore bool) {
	il := &instanceListener{dir: t.dir}
	il.container.Store(t.containerID)
	il.workspace.Store(t.workspace)
	l, err := listenInstanceSocket(t.dir, s.name, s.label, il, s.serve(key))

	s.mu.Lock()
	if err != nil {
		if !failedBefore {
			flog.Warn(s.label+" socket: listen failed", "instance", key, "err", err)
		}
		s.retryAt[key] = time.Now().Add(instanceListenRetry)
		s.mu.Unlock()
		return
	}
	delete(s.retryAt, key)
	il.socketListener = l
	if s.closed {
		s.mu.Unlock()
		l.Close()
		return
	}
	s.listeners[key] = il
	s.mu.Unlock()
}

// close stops every socket for good.
func (s *instanceSocketSet) close() {
	s.mu.Lock()
	s.closed = true
	listeners := make([]*instanceListener, 0, len(s.listeners))
	for key, il := range s.listeners {
		listeners = append(listeners, il)
		delete(s.listeners, key)
	}
	s.mu.Unlock()
	for _, il := range listeners {
		il.Close()
	}
}

// controlDirTarget returns an instance's target when its control directory
// exists on the host.
func controlDirTarget(fleetName string, inst *fleet.Instance) (instanceTarget, bool) {
	dir := state.ControlDir(fleetName, inst.Name)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return instanceTarget{}, false
	}
	return instanceTarget{dir: dir, containerID: inst.ContainerID, workspace: inst.WorkspaceDir}, true
}
