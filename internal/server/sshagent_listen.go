package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/create"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// sshagent_listen.go owns the relay's unix sockets:
//
//   - the HOST socket (agentsock.HostSocketPath, 0600 in a 0700 directory),
//     which the daemon points its own SSH_AUTH_SOCK at so every child it runs
//     — the repo clone, repo inspection, coder/codespaces `ssh -A` — reaches
//     the provider's agent;
//   - one socket per devcontainer instance, inside the instance's control
//     directory (already bind-mounted at /fleet-mounts/control), so processes
//     in the instance reach it at agentsock.ContainerSocketPath. Linux only
//     (agentsock.ModeRelay): on macOS a host socket cannot cross into Docker
//     Desktop's VM. The control directory is writable from the instance and
//     traversable by other host users, so these sockets are created without
//     resolving any path there (sshagent_peer_linux.go) and every connection
//     is checked: the daemon's user, root, or a process of that instance's
//     own container — never another host user.

const (
	// agentSyncInterval is how often the per-instance sockets are reconciled
	// against the instances in state.json.
	agentSyncInterval = 2 * time.Second
	// agentListenRetry spaces retries of an instance socket that failed to
	// listen (e.g. a path over the unix-socket length limit).
	agentListenRetry = 30 * time.Second
)

// agentListener is one relay socket.
type agentListener struct {
	ln   net.Listener
	path string
	// containerID names the container whose processes may connect besides the
	// daemon's user and root (nil for the host socket).
	containerID func() string
	// unlink removes the socket file on Close (nil: the listener's own
	// unlink-on-close does).
	unlink func()

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// listenAgentSocket serves the relay socket at path, handing each accepted
// connection to serve on its own goroutine. A stale SOCKET left at path (by a
// crashed daemon) is replaced; anything else there is left alone and fails.
func listenAgentSocket(path string, serve func(net.Conn)) (*agentListener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale agent socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod agent socket: %w", err)
	}
	l := &agentListener{ln: ln, path: path, done: make(chan struct{})}
	go l.acceptLoop(serve)
	return l, nil
}

func (l *agentListener) acceptLoop(serve func(net.Conn)) {
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
		containerID := ""
		if l.containerID != nil {
			containerID = l.containerID()
		}
		if !agentPeerAllowed(conn, containerID) {
			flog.Warn("ssh agent socket: refused a connection from another user", "socket", l.path)
			_ = conn.Close()
			continue
		}
		go serve(conn)
	}
}

// Close stops listening and removes the socket file.
func (l *agentListener) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.mu.Unlock()
	_ = l.ln.Close() // for the host socket this also unlinks the path
	<-l.done
	if l.unlink != nil {
		l.unlink()
	}
}

// track registers a live connection so shutdown can end it; false once the
// hub is closed.
func (h *agentHub) track(conn net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[conn] = struct{}{}
	h.serving.Add(1)
	return true
}

func (h *agentHub) untrack(conn net.Conn) {
	h.mu.Lock()
	delete(h.conns, conn)
	h.mu.Unlock()
	h.serving.Done()
}

// serveFrom returns the accept handler for a socket whose connections come
// from origin.
func (h *agentHub) serveFrom(origin string) func(net.Conn) {
	return func(conn net.Conn) {
		if !h.track(conn) {
			_ = conn.Close()
			return
		}
		defer h.untrack(conn)
		h.serveConn(conn, origin)
	}
}

// listenHost starts the host relay socket at path.
func (h *agentHub) listenHost(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	l, err := listenAgentSocket(path, h.serveFrom(""))
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.host = l
	h.mu.Unlock()
	return nil
}

// run reconciles the per-instance sockets until ctx is cancelled, then closes
// every socket and connection. Instances only get sockets in relay mode.
func (h *agentHub) run(ctx context.Context) {
	defer h.close()
	if agentsock.CurrentMode() != agentsock.ModeRelay {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(agentSyncInterval)
	defer ticker.Stop()
	for {
		if st, err := state.Load(); err == nil {
			h.syncInstances(st)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// instanceListener is a per-instance socket plus the container its
// processes run in (known once `devcontainer up` has returned).
type instanceListener struct {
	*agentListener
	dir         string
	containerID atomic.Value // string
}

func (il *instanceListener) currentContainerID() string {
	id, _ := il.containerID.Load().(string)
	return id
}

// syncInstances listens in the control directory of every instance that has
// one (devcontainer-style backends) and stops listening for instances that
// are gone. Status does not matter: a socket on a stopped instance costs
// nothing, and being there before a start means postStart can use it.
func (h *agentHub) syncInstances(st *state.State) {
	if st == nil {
		return
	}
	type wanted struct{ dir, containerID string }
	want := make(map[string]wanted)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			dir := state.ControlDir(fleetName, inst.Name)
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				want[fleetName+"/"+inst.Name] = wanted{dir: dir, containerID: inst.ContainerID}
			}
		}
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	var stale []*instanceListener
	for key, il := range h.instances {
		w, ok := want[key]
		if !ok || w.dir != il.dir || !il.stillBound() {
			stale = append(stale, il)
			delete(h.instances, key)
			continue
		}
		il.containerID.Store(w.containerID)
	}
	for key := range h.retryAt {
		if _, ok := want[key]; !ok {
			delete(h.retryAt, key)
		}
	}
	h.mu.Unlock()

	for _, il := range stale {
		il.Close()
	}
	for key, w := range want {
		h.openInstance(key, w.dir, w.containerID, false)
	}
}

// ensureInstance opens an instance's socket right away. Provisioning calls it
// (through create.ControlDirReady) as soon as the control directory exists,
// so a postCreate command in the new container already finds its agent
// rather than racing the next reconcile.
func (h *agentHub) ensureInstance(fleetName, instanceName string) {
	if agentsock.CurrentMode() != agentsock.ModeRelay {
		return
	}
	h.openInstance(fleetName+"/"+instanceName, state.ControlDir(fleetName, instanceName), "", true)
}

// openInstance listens for key unless it already is (or recently failed and
// is not due a retry, unless now is set).
func (h *agentHub) openInstance(key, dir, containerID string, now bool) {
	if !agentInstanceSocketsSupported {
		return
	}
	h.mu.Lock()
	_, listening := h.instances[key]
	at, failed := h.retryAt[key]
	if h.closed || listening || h.opening[key] || (failed && !now && time.Now().Before(at)) {
		h.mu.Unlock()
		return
	}
	h.opening[key] = true
	h.mu.Unlock()

	il := &instanceListener{dir: dir}
	il.containerID.Store(containerID)
	l, err := listenInstanceAgentSocket(dir, agentsock.SocketName, il.currentContainerID, h.serveFrom(key))

	h.mu.Lock()
	delete(h.opening, key)
	if err != nil {
		if !failed {
			flog.Warn("ssh agent socket: listen failed", "instance", key, "err", err)
		}
		h.retryAt[key] = time.Now().Add(agentListenRetry)
		h.mu.Unlock()
		return
	}
	delete(h.retryAt, key)
	il.agentListener = l
	if h.closed {
		h.mu.Unlock()
		l.Close()
		return
	}
	h.instances[key] = il
	h.mu.Unlock()
}

// stillBound reports whether the socket file is still there (an instance
// destroyed and re-created under the same name between reconciles, or a
// process in the instance deleting it, leaves this listener bound to nothing).
func (il *instanceListener) stillBound() bool {
	info, err := os.Lstat(il.path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// close stops every socket, ends every live connection, and waits for their
// goroutines.
func (h *agentHub) close() {
	h.mu.Lock()
	h.closed = true
	listeners := make([]*agentListener, 0, len(h.instances)+1)
	if h.host != nil {
		listeners = append(listeners, h.host)
		h.host = nil
	}
	for key, il := range h.instances {
		listeners = append(listeners, il.agentListener)
		delete(h.instances, key)
	}
	conns := make([]net.Conn, 0, len(h.conns))
	for conn := range h.conns {
		conns = append(conns, conn)
	}
	providers := h.providers
	h.providers = nil
	h.mu.Unlock()

	for _, l := range listeners {
		l.Close()
	}
	for _, p := range providers {
		p.finish()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	h.serving.Wait()
}

// startAgentRelay brings the relay up for the daemon's lifetime: it records
// the agent the daemon was started with as the fallback, listens on the host
// socket and — once that is up — points the daemon's own SSH_AUTH_SOCK at it,
// keeping the original in agentsock.EnvOrigin. Then it reconciles the
// per-instance sockets until ctx ends. Best-effort: if the host socket cannot
// be created, the daemon keeps its original agent and says so in the log.
func startAgentRelay(ctx context.Context, h *agentHub) {
	origin := agentsock.OriginSock()
	h.setFallback(origin)
	if agentsock.CurrentMode() != agentsock.ModeOff {
		path := agentsock.HostSocketPath()
		if err := h.listenHost(path); err != nil {
			flog.Warn("ssh agent relay: host socket unavailable; the daemon keeps its own agent", "err", err)
		} else {
			_ = os.Setenv(agentsock.EnvOrigin, origin)
			_ = os.Setenv(agentsock.EnvAuthSock, path)
			agentsock.SetRelayServing(true)
		}
	}
	create.ControlDirReady = h.ensureInstance
	go h.run(ctx)
}
