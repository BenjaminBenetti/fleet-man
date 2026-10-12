//go:build linux

package podagent

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// forwardDialTimeout bounds the agent's connect to a pod port.
const forwardDialTimeout = 10 * time.Second

// serveForward connects one TagForward stream to a port on the pod's
// loopback (IPv4, then IPv6 when nothing listens there, as the runtime's own
// port-forward does) and relays it both ways. Each direction ends on its
// own: fleetd's FIN shuts the TCP connection's write side while the peer's
// answer is still relayed, and the peer's EOF is fleetd's FIN, so a forward
// over the agent keeps a true half-close. fleetd's StreamAbort for the stream
// (a connection it closed for good) closes both at once, as does the end of
// the session.
func (a *agent) serveForward(s *podwire.Stream) {
	open := func(r podwire.OpenReply) error { return s.W.WriteJSON(podwire.FrameOpen, r) }
	var h podwire.ForwardHeader
	if err := podwire.DecodeHeader(s.Header, &h); err != nil {
		s.Penalize()
		_ = open(podwire.OpenReply{Error: err.Error()})
		return
	}
	if err := h.Validate(); err != nil {
		_ = open(podwire.OpenReply{Error: err.Error()})
		return
	}
	tcp, err := dialLoopback(a.ctx, h.Port)
	if err != nil {
		_ = open(podwire.OpenReply{Error: err.Error()})
		return
	}
	defer tcp.Close()
	stop := context.AfterFunc(a.ctx, func() { _ = tcp.Close() })
	defer stop()
	if err := open(podwire.OpenReply{}); err != nil {
		return
	}
	abort := a.trackForward(s.ID)
	defer abort.untrack()
	relayForward(s.Conn, tcp, abort.ch)
}

// forwardAbort is a relayed forward's StreamAbort: ch is closed when fleetd
// asks for its end.
type forwardAbort struct {
	ch      chan struct{}
	untrack func()
}

// trackForward registers forward stream id for fleetd's StreamAbort.
func (a *agent) trackForward(id uint32) forwardAbort {
	ch := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(ch) }) }
	a.fwdMu.Lock()
	if a.forwards == nil {
		a.forwards = map[uint32]func(){}
	}
	a.forwards[id] = end
	a.fwdMu.Unlock()
	return forwardAbort{ch: ch, untrack: func() {
		a.fwdMu.Lock()
		delete(a.forwards, id)
		a.fwdMu.Unlock()
	}}
}

// abortForward ends the forward of yamux stream id, if one is relayed.
func (a *agent) abortForward(id uint32) {
	a.fwdMu.Lock()
	end := a.forwards[id]
	a.fwdMu.Unlock()
	if end != nil {
		end()
	}
}

// relayForward copies a stream and a TCP connection both ways until both
// directions have ended, passing each EOF on as a half-close. When one side
// fails outright (not an EOF), or aborted is closed, both are closed.
func relayForward(stream net.Conn, tcp *net.TCPConn, aborted <-chan struct{}) {
	var wg sync.WaitGroup
	var closeBoth sync.Once
	abort := func() {
		closeBoth.Do(func() {
			_ = tcp.Close()
			// A FIN alone leaves the stream's read side open: the
			// deadline ends a read blocked on it.
			_ = stream.SetDeadline(time.Now())
			_ = stream.Close()
		})
	}
	relayed := make(chan struct{})
	defer close(relayed)
	go func() {
		select {
		case <-aborted:
			abort()
		case <-relayed:
		}
	}()
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(tcp, stream); err != nil {
			abort()
			return
		}
		// fleetd's FIN: the peer reads EOF and may still answer.
		_ = tcp.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(stream, tcp); err != nil && !errors.Is(err, net.ErrClosed) {
			abort()
			return
		}
		// The peer is done: a yamux Close is the stream's FIN, its read
		// side stays open for what fleetd still sends.
		_ = stream.Close()
	}()
	wg.Wait()
}

// dialLoopback connects to port on 127.0.0.1, then on ::1 when the first is
// refused.
func dialLoopback(ctx context.Context, port int) (*net.TCPConn, error) {
	d := net.Dialer{Timeout: forwardDialTimeout}
	var first error
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			return c.(*net.TCPConn), nil
		}
		if first == nil {
			first = err
		}
		if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.EADDRNOTAVAIL) && !errors.Is(err, syscall.ENETUNREACH) {
			break
		}
	}
	return nil, first
}
