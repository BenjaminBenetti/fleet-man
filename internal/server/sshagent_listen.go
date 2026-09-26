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
	// inst identifies the instance whose container's processes may connect
	// besides the daemon's user and root (nil for the host socket).
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

// maxSlowPeerChecks is how many cross-uid peer checks (which may run docker)
// one socket runs at once; more are refused.
const maxSlowPeerChecks = 8

// refusalLogInterval spaces "refused a connection" warnings for one socket.
const refusalLogInterval = time.Minute

func (l *agentListener) refused() {
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
	flog.Warn("ssh agent socket: refused connections from another user", "socket", l.path, "count", count)
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
		// The peer check may ask docker (cached): never on the accept loop,
		// where one slow answer would hold up every other connection.
		go func() {
			if !agentPeerAllowed(conn, l.inst, l.slowChecks) {
				l.refused()
				_ = conn.Close()
				return
			}
			serve(conn)
		}()
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

// run reconciles the per-instance sockets (relay mode) and refreshes the
// published relay verdict until ctx is cancelled, then closes every socket and
// connection.
func (h *agentHub) run(ctx context.Context) {
	defer h.close()
	relay := agentsock.CurrentMode() == agentsock.ModeRelay
	ticker := time.NewTicker(agentSyncInterval)
	defer ticker.Stop()
	for {
		if relay {
			if st, err := state.Load(); err == nil {
				h.syncInstances(st)
			}
		}
		// Whether the daemon's own agent is alive changes on its own (a
		// login's forwarded agent goes away): keep the verdict that
		// in-process CLI backends read current, and redirect once usable.
		agentsock.RefreshUsable()
		h.maybeRedirect()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// maybeRedirect points the daemon's own SSH_AUTH_SOCK at the relay once
// something can answer there (agentsock.RelayUsable). No-op before
// startAgentRelay has set it up, and once done.
func (h *agentHub) maybeRedirect() {
	h.mu.Lock()
	redirect := h.redirect
	h.mu.Unlock()
	if redirect != nil && agentsock.RelayUsable() {
		redirect()
	}
}

// instanceListener is a per-instance socket plus what identifies its
// container: the recorded ID (known once `devcontainer up` has returned) and
// the workspace folder its container is labelled with.
type instanceListener struct {
	*agentListener
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

// syncInstances listens in the control directory of every instance that has
// one (devcontainer-style backends) and stops listening for instances that
// are gone. Status does not matter: a socket on a stopped instance costs
// nothing, and being there before a start means postStart can use it.
func (h *agentHub) syncInstances(st *state.State) {
	if st == nil {
		return
	}
	type wanted struct{ dir, containerID, workspace string }
	want := make(map[string]wanted)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			dir := state.ControlDir(fleetName, inst.Name)
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				want[fleetName+"/"+inst.Name] = wanted{dir: dir, containerID: inst.ContainerID, workspace: inst.WorkspaceDir}
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
		il.container.Store(w.containerID)
		il.workspace.Store(w.workspace)
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
		h.openInstance(key, w.dir, w.containerID, w.workspace, false)
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
	// The record already exists (provisioning runs for a StatusCreating
	// instance); its workspace folder is how the container is recognized
	// before its ID is recorded.
	var containerID, workspace string
	if st, err := state.Load(); err == nil {
		if f := st.Fleets[fleetName]; f != nil {
			for _, inst := range f.Instances {
				if inst.Name == instanceName {
					containerID, workspace = inst.ContainerID, inst.WorkspaceDir
				}
			}
		}
	}
	h.openInstance(fleetName+"/"+instanceName, state.ControlDir(fleetName, instanceName), containerID, workspace, true)
}

// openInstance listens for key unless it already is (or recently failed and
// is not due a retry, unless now is set).
func (h *agentHub) openInstance(key, dir, containerID, workspace string, now bool) {
	if !agentInstanceSocketsSupported {
		return
	}
	for {
		h.mu.Lock()
		_, listening := h.instances[key]
		at, failed := h.retryAt[key]
		if h.closed || listening || (failed && !now && time.Now().Before(at)) {
			h.mu.Unlock()
			return
		}
		if inflight, busy := h.opening[key]; busy {
			h.mu.Unlock()
			if !now {
				return
			}
			// The provisioning hook promises the socket exists when it
			// returns: wait for a reconcile's open of the same instance, and
			// try again at once if that one failed.
			<-inflight
			continue
		}
		settled := make(chan struct{})
		h.opening[key] = settled
		h.mu.Unlock()

		h.listenInstance(key, dir, containerID, workspace, failed)

		h.mu.Lock()
		delete(h.opening, key)
		close(settled)
		h.mu.Unlock()
		return
	}
}

// listenInstance opens one instance's socket for openInstance, which holds
// its slot in h.opening.
func (h *agentHub) listenInstance(key, dir, containerID, workspace string, failedBefore bool) {
	il := &instanceListener{dir: dir}
	il.container.Store(containerID)
	il.workspace.Store(workspace)
	l, err := listenInstanceAgentSocket(dir, agentsock.SocketName, il, h.serveFrom(key))

	h.mu.Lock()
	if err != nil {
		if !failedBefore {
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

// close stops every socket, ends every live connection, and waits for their
// goroutines.
func (h *agentHub) close() {
	h.mu.Lock()
	h.closed = true
	listeners := make([]*agentListener, 0, len(h.instances)+1)
	relayStarted := h.relayStarted
	h.relayStarted = false
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
	if len(providers) > 0 {
		agentsock.SetForwarded(false)
	}
	h.mu.Unlock()

	if relayStarted {
		agentsock.SetRelayServing(false)
	}
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
// socket, and points the daemon's own SSH_AUTH_SOCK at it (keeping the
// original in agentsock.EnvOrigin) — right away if the daemon has an agent of
// its own or a client has provided one before, otherwise as soon as the first
// provider attaches, so the host's own scripts keep an unset SSH_AUTH_SOCK
// until something can answer on the relay. Then it reconciles the
// per-instance sockets until ctx ends. Best-effort: if the host socket cannot
// be created, the daemon keeps its original agent and says so in the log.
func startAgentRelay(ctx context.Context, h *agentHub) (done <-chan struct{}) {
	origin := agentsock.OriginSock()
	h.setFallback(origin)
	if agentsock.CurrentMode() != agentsock.ModeOff {
		// The instance sockets are served whether or not the host one can
		// be: they are bound through /proc/self/fd, clear of the unix socket
		// path limit a long HOME puts the host socket over.
		_, err := os.Stat(agentsock.ProviderSeenPath())
		agentsock.SetProviderSeen(err == nil)
		agentsock.SetRelayServing(true)
		h.mu.Lock()
		h.relayStarted = true
		h.onFirstProvider = func() {
			if f, err := os.OpenFile(agentsock.ProviderSeenPath(), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_ = f.Close()
			}
			agentsock.SetProviderSeen(true)
			h.maybeRedirect()
		}
		h.mu.Unlock()
		path := agentsock.HostSocketPath()
		if err := h.listenHost(path); err != nil {
			flog.Warn("ssh agent relay: host socket unavailable; the daemon keeps its own agent", "err", err)
		} else {
			var once sync.Once
			h.mu.Lock()
			h.redirect = func() {
				once.Do(func() {
					_ = os.Setenv(agentsock.EnvOrigin, origin)
					_ = os.Setenv(agentsock.EnvAuthSock, path)
				})
			}
			h.mu.Unlock()
			// Remote Fleet is not known yet (the config is reconciled after
			// this): with only a provider seen before, reconcileRemote
			// completes the redirect.
			h.maybeRedirect()
		}
	}
	create.ControlDirReady = h.ensureInstance
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.run(ctx)
	}()
	return finished
}
