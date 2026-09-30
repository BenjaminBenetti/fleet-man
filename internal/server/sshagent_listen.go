package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
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
//     Desktop's VM. They are an instanceSocketSet (instance_sockets.go), which
//     binds them safely and checks every connection's peer.

// agentSyncInterval is how often the per-instance sockets are reconciled
// against the instances in state.json.
const agentSyncInterval = 2 * time.Second

// listenAgentSocket serves the relay socket at path, handing each accepted
// connection to serve on its own goroutine. A stale SOCKET left at path (by a
// crashed daemon) is replaced; anything else there is left alone and fails.
func listenAgentSocket(path string, serve func(net.Conn)) (*socketListener, error) {
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
	l := &socketListener{ln: ln, path: path, label: agentSocketLabel, done: make(chan struct{})}
	go l.acceptLoop(serve)
	return l, nil
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

// syncInstances listens in the control directory of every instance that has
// one (devcontainer-style backends) and stops listening for instances that
// are gone. Status does not matter: a socket on a stopped instance costs
// nothing, and being there before a start means postStart can use it.
func (h *agentHub) syncInstances(st *state.State) {
	if st == nil {
		return
	}
	want := make(map[string]instanceTarget)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if t, ok := controlDirTarget(fleetName, inst); ok {
				want[fleetName+"/"+inst.Name] = t
			}
		}
	}
	h.sockets.sync(want)
}

// ensureInstance opens an instance's socket right away. Provisioning calls it
// (through create.ControlDirReady) as soon as the control directory exists,
// so a postCreate command in the new container already finds its agent
// rather than racing the next reconcile.
func (h *agentHub) ensureInstance(fleetName, instanceName string) {
	if agentsock.CurrentMode() != agentsock.ModeRelay {
		return
	}
	h.sockets.ensure(fleetName+"/"+instanceName, loadInstanceTarget(fleetName, instanceName))
}

// close stops every socket, ends every live connection, and waits for their
// goroutines.
func (h *agentHub) close() {
	h.mu.Lock()
	h.closed = true
	relayStarted := h.relayStarted
	h.relayStarted = false
	host := h.host
	h.host = nil
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
	if host != nil {
		host.Close()
	}
	h.sockets.close()
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
