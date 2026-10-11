//go:build linux

package podagent

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// gateway_linux.go mirrors the daemon host's listening TCP ports in the pod,
// for a devcontainer that declares host-gateway: a listener on
// podwire.HostGatewayAddr (where host.docker.internal resolves in the pod) at
// each port the DesiredState names, whose every connection is relayed to
// fleetd on a TagHostDial stream naming the port; fleetd dials the port on
// the daemon host.
//
// A port the pod itself listens at on every interface cannot be mirrored
// (the bind fails as in use) and is left to the pod; it is tried again on
// every check, in case the pod's listener goes. The other way round, a pod
// process that would listen on every interface at a port the mirror holds
// finds it in use: the create's warning says so.

// mirrorPort is one mirrored daemon-host port.
type mirrorPort struct {
	port uint16
	ln   *net.TCPListener
	done chan struct{}
}

// syncGatewayLocked listens at each port of want that is not mirrored yet
// and stops the mirrors of the others.
func (c *controlDir) syncGatewayLocked(want []uint16) {
	keep := make(map[uint16]bool, len(want))
	for _, p := range want {
		keep[p] = true
	}
	for p := range c.gateway {
		if !keep[p] {
			c.unbindPortLocked(p)
		}
	}
	for what := range c.failed {
		if p, ok := gatewayFailure(what); ok && !keep[p] {
			delete(c.failed, what)
		}
	}
	for _, p := range want {
		if c.gateway[p] == nil {
			c.bindPortLocked(p)
		}
	}
}

// gatewayKey is a gateway port's key in c.failed.
func gatewayKey(p uint16) string { return "gateway port " + strconv.Itoa(int(p)) }

func gatewayFailure(what string) (uint16, bool) {
	const prefix = "gateway port "
	if len(what) <= len(prefix) || what[:len(prefix)] != prefix {
		return 0, false
	}
	n, err := strconv.ParseUint(what[len(prefix):], 10, 16)
	return uint16(n), err == nil
}

// bindPortLocked listens at HostGatewayAddr:p. A port in use in the pod is
// the pod's (quietly left to it); another failure is logged once, at warn:
// it costs the pod that one port of the daemon host, not its control
// directory.
func (c *controlDir) bindPortLocked(p uint16) {
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP(podwire.HostGatewayAddr), Port: int(p)})
	if err != nil {
		key := gatewayKey(p)
		if errors.Is(err, syscall.EADDRINUSE) {
			delete(c.failed, key)
			return
		}
		if c.failed[key] != err.Error() {
			c.failed[key] = err.Error()
			c.log.Warn("could not mirror a daemon-host port", "port", strconv.Itoa(int(p)), "err", err.Error())
		}
		return
	}
	delete(c.failed, gatewayKey(p))
	m := &mirrorPort{port: p, ln: ln, done: make(chan struct{})}
	c.gateway[p] = m
	go c.acceptTCP(m)
}

// unbindPortLocked stops mirroring p.
func (c *controlDir) unbindPortLocked(p uint16) {
	m := c.gateway[p]
	if m == nil {
		return
	}
	delete(c.gateway, p)
	_ = m.ln.Close()
	<-m.done
}

// acceptTCP relays every connection to m until it is closed.
func (c *controlDir) acceptTCP(m *mirrorPort) {
	defer close(m.done)
	for {
		conn, err := m.ln.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !c.track(conn) {
			_ = conn.Close()
			continue
		}
		go c.relayTCP(m.port, conn)
	}
}

// relayTCP carries one connection to a mirrored port to fleetd. One fleetd
// does not serve (the daemon host no longer listens there) is reset, as a
// refused connection would be on docker, not ended as if served.
func (c *controlDir) relayTCP(port uint16, conn *net.TCPConn) {
	defer c.untrack(conn)
	defer conn.Close()
	up, err := c.dial(podwire.TagHostDial, podwire.SockOpen{Port: port})
	if err != nil {
		_ = conn.SetLinger(0)
		if !podwire.IsSockRefused(err, "") {
			c.log.Warn("could not relay a connection to fleetd", "port", strconv.Itoa(int(port)), "err", err.Error())
		}
		return
	}
	splice(conn, up)
}
