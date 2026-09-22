// Package agentfwd is the CLIENT half of SSH-agent forwarding: it runs where
// the human's ssh-agent is (the TUI, a CLI command) and provides that agent to
// a fleet daemon — typically a remote one — over the SSHAgent stream. The
// daemon relays every agent connection made on its host (its own git clone,
// processes inside instances) down the stream; each one is spliced onto the
// local SSH_AUTH_SOCK here. The effect is `ssh -A` for everything the daemon
// runs, lasting exactly as long as this provider does.
//
// It is client code (depguard-checked): it talks to the daemon only through
// fleetgrpc.
package agentfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// State is the provider's coarse condition, for the UI.
type State int

const (
	// StateConnecting: no stream to the daemon yet (or it dropped).
	StateConnecting State = iota
	// StateActive: attached, and this machine's agent is the one in use.
	StateActive
	// StateStandby: attached, but a newer provider (another client) is
	// active; this one takes over if it leaves.
	StateStandby
	// StateNoAgent: this machine has no reachable agent (SSH_AUTH_SOCK unset or
	// dead), so the provider stays detached — attaching would only put a
	// useless provider in front of the daemon's own agent. Re-checked.
	StateNoAgent
	// StateUnsupported: the daemon does not know the RPC (older fleetd).
	// Terminal — Run returns.
	StateUnsupported
	// StateRefused: the daemon's host has agent forwarding turned off
	// (FLEET_SSH_AGENT_SOCK=off there). Retried slowly.
	StateRefused
)

// Status is one provider status report.
type Status struct {
	State State
	// Uses counts the agent connections served since this provider started.
	Uses int
	// Detail carries the reason for StateNoAgent / StateRefused.
	Detail string
}

const (
	reconnectInitial = 500 * time.Millisecond
	reconnectMax     = 10 * time.Second
	refusedRetry     = 30 * time.Second
	noAgentRetry     = 5 * time.Second
	// streamHealthy is how long a stream must last before the reconnect
	// back-off is forgiven (a peer that accepts and drops at once must not be
	// redialed at 2 Hz forever).
	streamHealthy = 10 * time.Second
	// agentDialTimeout bounds dialing the local agent.
	agentDialTimeout = 3 * time.Second
	// readChunk is how much of the local agent's reply is sent per frame.
	readChunk = 32 * 1024
	// connQueue bounds daemon→agent frames waiting for one connection.
	connQueue = 64
	// sendQueue buffers frames waiting for the stream.
	sendQueue = 64
	// maxFrameBytes rejects absurd frames from the daemon (OpenSSH caps an
	// agent message at 256 KiB).
	maxFrameBytes = 1 << 20
)

// Seams for tests.
var (
	// agentSocket reads the local agent's socket path — per connection, so an
	// agent restarted at a new path is picked up without restarting anything.
	agentSocket = func() string { return os.Getenv("SSH_AUTH_SOCK") }
	// hostname labels this provider in the daemon's log.
	hostname = func() string {
		name, err := os.Hostname()
		if err != nil {
			return "unknown"
		}
		return name
	}
)

// AgentReachable reports whether this machine has a usable agent right now.
func AgentReachable() error {
	sock := agentSocket()
	if sock == "" {
		return errors.New("SSH_AUTH_SOCK is not set")
	}
	conn, err := net.DialTimeout("unix", sock, agentDialTimeout)
	if err != nil {
		return fmt.Errorf("cannot reach the ssh-agent at %s: %w", sock, err)
	}
	_ = conn.Close()
	return nil
}

// Run is the provider loop: it holds an SSHAgent stream to the daemon and
// serves the connections the daemon announces from the local agent. It
// reconnects with backoff and returns when ctx is cancelled (or the daemon
// does not support the RPC). report is called from Run's goroutines on every
// status change; it must not block.
func Run(ctx context.Context, svc fleetgrpc.FleetServiceClient, report func(Status)) {
	r := &runner{report: report}
	backoff := reconnectInitial
	for ctx.Err() == nil {
		if err := AgentReachable(); err != nil {
			r.set(StateNoAgent, err.Error())
			if !sleep(ctx, noAgentRetry) {
				return
			}
			continue
		}
		r.set(StateConnecting, "")
		began := time.Now()
		attached, err := r.runStream(ctx, svc)
		wait := backoff
		switch status.Code(err) {
		case codes.Unimplemented:
			r.set(StateUnsupported, "")
			return
		case codes.FailedPrecondition:
			r.set(StateRefused, status.Convert(err).Message())
			wait = refusedRetry
		default:
			if attached && time.Since(began) >= streamHealthy {
				backoff = reconnectInitial
				wait = backoff
			} else {
				backoff = min(backoff*2, reconnectMax)
			}
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// runner carries the status across streams (Uses keeps counting).
type runner struct {
	report func(Status)
	mu     sync.Mutex
	state  State
	uses   int
	detail string
}

// set and used report under the lock so reports from the stream loop and the
// connection goroutines arrive in the order the state changed (report never
// blocks, by contract).
func (r *runner) set(state State, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state, r.detail = state, detail
	r.report(Status{State: r.state, Uses: r.uses, Detail: r.detail})
}

func (r *runner) used() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uses++
	r.report(Status{State: r.state, Uses: r.uses, Detail: r.detail})
}

// runStream runs one stream to completion. attached reports whether the
// daemon accepted it.
func (r *runner) runStream(ctx context.Context, svc fleetgrpc.FleetServiceClient) (attached bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := svc.SSHAgent(ctx, grpc.WaitForReady(true))
	if err != nil {
		return false, err
	}
	if err := stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Hello{Hello: &fleetgrpc.SSHAgentHello{Client: hostname()}}}); err != nil {
		return false, err
	}

	s := &session{
		ctx:   ctx,
		out:   make(chan *fleetgrpc.SSHAgentUp, sendQueue),
		conns: make(map[uint64]*localConn),
		used:  r.used,
	}
	defer s.closeAll()

	// ONE sender for the stream's life: Send is not safe for concurrent use,
	// and every connection's reader funnels through here.
	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case msg := <-s.out:
				if err := stream.Send(msg); err != nil {
					sendErr <- err
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		down, err := stream.Recv()
		if err != nil {
			select {
			case serr := <-sendErr:
				if !errors.Is(serr, io.EOF) {
					err = serr
				}
			default:
			}
			return attached, err
		}
		switch msg := down.GetMsg().(type) {
		case *fleetgrpc.SSHAgentDown_Status:
			attached = true
			if msg.Status.GetActive() {
				r.set(StateActive, "")
			} else {
				r.set(StateStandby, "")
			}
		case *fleetgrpc.SSHAgentDown_Open:
			attached = true
			go s.open(msg.Open.GetConnId())
		case *fleetgrpc.SSHAgentDown_Data:
			s.data(msg.Data.GetConnId(), msg.Data.GetData())
		case *fleetgrpc.SSHAgentDown_Close:
			s.closeConn(msg.Close.GetConnId())
		}
	}
}

// session is one stream's set of spliced connections.
type session struct {
	ctx  context.Context
	out  chan *fleetgrpc.SSHAgentUp
	used func()

	mu    sync.Mutex
	conns map[uint64]*localConn
	done  bool
}

// localConn is one daemon connection spliced onto the local agent.
type localConn struct {
	conn      net.Conn
	in        chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *localConn) shut() {
	c.closeOnce.Do(func() { close(c.closed) })
}

// send queues a frame for the daemon; false once the stream is over.
func (s *session) send(msg *fleetgrpc.SSHAgentUp) bool {
	select {
	case s.out <- msg:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func upClose(id uint64, errMsg string) *fleetgrpc.SSHAgentUp {
	return &fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Close{Close: &fleetgrpc.SSHAgentClose{ConnId: id, Error: errMsg}}}
}

// open dials the local agent for connection id and splices the two until
// either side ends it.
func (s *session) open(id uint64) {
	sock := agentSocket()
	if sock == "" {
		s.send(upClose(id, "SSH_AUTH_SOCK is not set on "+hostname()))
		return
	}
	agentConn, err := net.DialTimeout("unix", sock, agentDialTimeout)
	if err != nil {
		s.send(upClose(id, fmt.Sprintf("cannot reach the ssh-agent on %s: %v", hostname(), err)))
		return
	}
	lc := &localConn{conn: agentConn, in: make(chan []byte, connQueue), closed: make(chan struct{})}
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		_ = agentConn.Close()
		return
	}
	s.conns[id] = lc
	s.mu.Unlock()
	defer s.forget(id)

	if !s.send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Ready{Ready: &fleetgrpc.SSHAgentReady{ConnId: id}}}) {
		_ = agentConn.Close()
		return
	}
	s.used()

	// daemon → agent. Frames queued before a close are written first.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer agentConn.Close()
		for {
			select {
			case data := <-lc.in:
				if _, err := agentConn.Write(data); err != nil {
					return
				}
			case <-lc.closed:
				for {
					select {
					case data := <-lc.in:
						if _, err := agentConn.Write(data); err != nil {
							return
						}
					default:
						return
					}
				}
			case <-s.ctx.Done():
				return
			}
		}
	}()

	// agent → daemon.
	buf := make([]byte, readChunk)
	for {
		n, err := agentConn.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if !s.send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Data{Data: &fleetgrpc.SSHAgentData{ConnId: id, Data: data}}}) {
				break
			}
		}
		if err != nil {
			select {
			case <-lc.closed: // the daemon ended it
			default:
				s.send(upClose(id, ""))
			}
			break
		}
	}
	lc.shut()
	<-writerDone
}

func (s *session) lookup(id uint64) *localConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id]
}

func (s *session) forget(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, id)
}

// data hands bytes from the daemon to connection id.
func (s *session) data(id uint64, data []byte) {
	lc := s.lookup(id)
	if lc == nil || len(data) == 0 {
		return
	}
	if len(data) > maxFrameBytes {
		s.closeConn(id)
		s.send(upClose(id, ""))
		return
	}
	select {
	case lc.in <- data:
	default:
		// The local agent stopped reading; end this connection rather than
		// stall every other one on the stream.
		s.closeConn(id)
		s.send(upClose(id, ""))
	}
}

// closeConn ends connection id at the daemon's request.
func (s *session) closeConn(id uint64) {
	if lc := s.lookup(id); lc != nil {
		lc.shut()
	}
}

// closeAll ends every connection (the stream is over).
func (s *session) closeAll() {
	s.mu.Lock()
	s.done = true
	conns := s.conns
	s.conns = make(map[uint64]*localConn)
	s.mu.Unlock()
	for _, lc := range conns {
		lc.shut()
		_ = lc.conn.Close()
	}
}
