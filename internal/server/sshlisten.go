package server

import (
	"errors"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"google.golang.org/grpc"
)

// sshlisten.go is the daemon side of "Remote Fleet via SSH": while remote fleet
// is enabled in SSH mode, the SAME token-gated FleetService the gateway tunnel
// serves is ALSO served on a loopback TCP port, and the port is recorded in
// ~/.fleet/ssh.port. A remote client's local daemon ssh-forwards to that port
// (internal/server/sshtunnel) after reading the port + bearer token over SSH —
// no gateway, no session ids. Loopback + bearer token is the same posture as
// the MCP HTTP server: any local user can reach the port, only the token holder
// gets in. The listener is built fresh on every enable (a grpc.Server can't
// stop serving ONE of its listeners while keeping others), and Stop drops its
// live connections, so a disable cuts remote control immediately.

// sshListener converges the loopback listener on a desired on/off state.
type sshListener struct {
	// newServer builds a token-gated grpc.Server with the FleetService
	// registered (server.go wires it with the bearer interceptors). It returns an
	// error when the bearer token cannot be loaded — then there is nothing safe
	// to serve, and the error is what the settings page shows.
	newServer func() (*grpc.Server, error)
	// publish reports the listener's state to the hub: addr while listening ("" when
	// off), errMsg while enabled-but-failed.
	publish func(addr, errMsg string)

	mu  sync.Mutex
	srv *grpc.Server
	lis net.Listener
}

// Reconcile converges to enabled. Idempotent and cheap on no change, so it is
// safe to call on every SetConfig (like remote.Manager.Reconcile).
func (l *sshListener) Reconcile(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case enabled && l.srv == nil:
		l.startLocked()
	case !enabled && l.srv != nil:
		l.stopLocked()
		l.publish("", "")
	}
}

// Stop tears the listener down (daemon shutdown). Also removes the port file
// so a discovery script never sees a stale hint.
func (l *sshListener) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.srv != nil {
		l.stopLocked()
	}
}

func (l *sshListener) startLocked() {
	srv, err := l.newServer()
	if err != nil {
		flog.Warn("remote fleet via ssh: build server", "err", err)
		l.publish("", err.Error())
		return
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		flog.Warn("remote fleet via ssh: listen", "err", err)
		l.publish("", "listen: "+err.Error())
		return
	}
	port := lis.Addr().(*net.TCPAddr).Port
	// The port file is the remote client's discovery hint (read over SSH with
	// mcp.token). 0o600 like the other ~/.fleet discovery files. ~/.fleet exists
	// by the time Serve runs the first Reconcile, but ensure it (cheap) so a
	// test or an early caller can't fail on a missing parent.
	if _, err := fleetpaths.EnsureDir(); err != nil {
		flog.Warn("remote fleet via ssh: ensure fleet dir", "err", err)
		_ = lis.Close()
		l.publish("", "ensure fleet dir: "+err.Error())
		return
	}
	if err := atomicfile.Write(fleetpaths.SSHPortPath(), []byte(strconv.Itoa(port)), 0o600); err != nil {
		flog.Warn("remote fleet via ssh: write ssh.port", "err", err)
		_ = lis.Close()
		l.publish("", "write ssh.port: "+err.Error())
		return
	}
	l.srv, l.lis = srv, lis
	go func() { _ = srv.Serve(lis) }()
	flog.Info("remote fleet via ssh listening", "addr", lis.Addr().String())
	l.publish(lis.Addr().String(), "")
}

func (l *sshListener) stopLocked() {
	l.srv.Stop() // closes the listener and every live connection
	l.srv, l.lis = nil, nil
	if err := os.Remove(fleetpaths.SSHPortPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		flog.Warn("remote fleet via ssh: remove ssh.port", "err", err)
	}
	flog.Info("remote fleet via ssh stopped")
}
