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
	"github.com/BenjaminBenetti/fleet-man/internal/agentproto"
	"golang.org/x/crypto/ssh"
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
	// dead). Before attaching, the provider then stays detached — attaching
	// would only put a useless provider in front of the daemon's own agent —
	// and re-checks; while attached, it is reported when an agent dial fails
	// (the daemon then tries the next provider) and cleared by the next
	// successful one.
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

// agentSocketFn reads the local agent's socket path — per connection, so an
// agent restarted at the same path is picked up without restarting anything.
// Behind a lock because tests swap it while an earlier provider's goroutines
// may still be reading it.
var (
	agentSocketMu sync.RWMutex
	agentSocketFn = func() string { return os.Getenv("SSH_AUTH_SOCK") }
)

func agentSocket() string {
	agentSocketMu.RLock()
	fn := agentSocketFn
	agentSocketMu.RUnlock()
	return fn()
}

// Seams for tests.
var (
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
// does not support the RPC). role names this provider in the daemon's log
// ("tui", "cli"). report is called on every status change, possibly from
// several goroutines but never concurrently; it must not block.
func Run(ctx context.Context, svc fleetgrpc.FleetServiceClient, role string, report func(Status)) {
	r := &runner{report: report, label: hostname() + " (" + role + ")", yield: role != "tui"}
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
	label  string
	// yield: a CLI command's provider queues behind the TUI's (see Hello).
	yield bool
	// bindKey is this provider's throwaway "host key" for forwarding binds.
	bindOnce sync.Once
	bindKey  ssh.Signer
	mu       sync.Mutex
	state    State
	uses     int
	detail   string
	// active is the daemon's last word on this provider (newest or standing
	// by), restored when a connection succeeds after the agent was missing.
	active bool
}

// key returns the provider's forwarding-bind key, made on first use.
func (r *runner) key() ssh.Signer {
	r.bindOnce.Do(func() { r.bindKey, _ = agentproto.NewBindKey() })
	return r.bindKey
}

// set and the helpers below report under the lock so reports from the stream
// loop and the connection goroutines arrive in the order the state changed
// (report never blocks, by contract).
func (r *runner) set(state State, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state, r.detail = state, detail
	r.emit()
}

func (r *runner) emit() {
	r.report(Status{State: r.state, Uses: r.uses, Detail: r.detail})
}

// attachedAs records the daemon's status message.
func (r *runner) attachedAs(active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = active
	r.state, r.detail = StateStandby, ""
	if active {
		r.state = StateActive
	}
	r.emit()
}

// served counts a connection spliced onto the agent; it also clears a
// "no local agent" state left by an earlier failed dial.
func (r *runner) served() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uses++
	if r.state == StateNoAgent {
		r.state, r.detail = StateStandby, ""
		if r.active {
			r.state = StateActive
		}
	}
	r.emit()
}

// agentMissing reports, while attached, that the local agent could not be
// dialed for a connection (the daemon then tries elsewhere): the status must
// not keep claiming "forwarding".
func (r *runner) agentMissing(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state, r.detail = StateNoAgent, detail
	r.emit()
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
	if err := stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Hello{Hello: &fleetgrpc.SSHAgentHello{Client: r.label, Yield: r.yield}}}); err != nil {
		return false, err
	}

	s := &session{
		ctx:    ctx,
		out:    make(chan *fleetgrpc.SSHAgentUp, sendQueue),
		conns:  make(map[uint64]*localConn),
		runner: r,
	}
	defer s.closeAll()

	// ONE sender for the stream's life: Send is not safe for concurrent use,
	// and every connection funnels through here.
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
			r.attachedAs(msg.Status.GetActive())
		case *fleetgrpc.SSHAgentDown_Ping:
			// Never block this loop on a congested send queue: a missed pong
			// only costs a later ping.
			select {
			case s.out <- &fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Pong{Pong: &fleetgrpc.SSHAgentPong{Seq: msg.Ping.GetSeq()}}}:
			default:
			}
		case *fleetgrpc.SSHAgentDown_Open:
			attached = true
			// Registered HERE, before the goroutine dials, so a close the
			// daemon sends right behind the open (it gave up waiting) finds
			// the connection and cancels it rather than being dropped.
			if lc := s.register(msg.Open.GetConnId()); lc != nil {
				go s.serve(msg.Open.GetConnId(), lc)
			}
		case *fleetgrpc.SSHAgentDown_Data:
			s.data(msg.Data.GetConnId(), msg.Data.GetData())
		case *fleetgrpc.SSHAgentDown_Close:
			s.peerClosed(msg.Close.GetConnId())
		}
	}
}

// session is one stream's set of spliced connections.
type session struct {
	ctx    context.Context
	out    chan *fleetgrpc.SSHAgentUp
	runner *runner

	mu    sync.Mutex
	conns map[uint64]*localConn
	done  bool
}

// localConn is one daemon connection being served from the local agent.
type localConn struct {
	// in carries request bytes from the daemon.
	in chan []byte
	// peerDone closes when the daemon has nothing more to send (its close).
	peerDone chan struct{}
	peerOnce sync.Once
	// agent is the dialed agent connection (nil until dialed).
	agentMu sync.Mutex
	agent   net.Conn
	aborted bool
}

func (c *localConn) finishPeer() {
	c.peerOnce.Do(func() { close(c.peerDone) })
}

// setAgent records the dialed agent connection; false if the connection was
// aborted meanwhile (the caller must close agent).
func (c *localConn) setAgent(agent net.Conn) bool {
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	if c.aborted {
		return false
	}
	c.agent = agent
	return true
}

// abort ends the connection now: the agent connection is closed so a pending
// read returns.
func (c *localConn) abort() {
	c.agentMu.Lock()
	c.aborted = true
	agent := c.agent
	c.agentMu.Unlock()
	c.finishPeer()
	if agent != nil {
		_ = agent.Close()
	}
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

func upData(id uint64, data []byte) *fleetgrpc.SSHAgentUp {
	return &fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Data{Data: &fleetgrpc.SSHAgentData{ConnId: id, Data: data}}}
}

// register records a new connection; nil once the stream is over.
func (s *session) register(id uint64) *localConn {
	lc := &localConn{in: make(chan []byte, connQueue), peerDone: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.conns[id] = lc
	return lc
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

// serve dials the local agent for connection id and answers the daemon's
// requests one at a time — each allowed request is forwarded and its reply
// read back before the next is looked at, so replies stay in order even when
// some requests are answered here instead of by the agent.
func (s *session) serve(id uint64, lc *localConn) {
	defer s.forget(id)
	sock := agentSocket()
	if sock == "" {
		s.runner.agentMissing("SSH_AUTH_SOCK is not set")
		s.send(upClose(id, "SSH_AUTH_SOCK is not set on "+hostname()))
		return
	}
	agent, err := net.DialTimeout("unix", sock, agentDialTimeout)
	if err != nil {
		s.runner.agentMissing(fmt.Sprintf("cannot reach the ssh-agent at %s", sock))
		s.send(upClose(id, fmt.Sprintf("cannot reach the ssh-agent on %s: %v", hostname(), err)))
		return
	}
	if !lc.setAgent(agent) {
		_ = agent.Close()
		return
	}
	defer agent.Close()
	select {
	case <-lc.peerDone:
		// The daemon closed before any byte could flow: it gave up waiting
		// for this answer. Nothing to say.
		return
	default:
	}
	agentproto.BindAsForwarded(agent, s.runner.key())
	if !s.send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Ready{Ready: &fleetgrpc.SSHAgentReady{ConnId: id}}}) {
		return
	}
	s.runner.served()

	requests := &agentproto.MessageReader{Next: func() ([]byte, bool) {
		select {
		case chunk := <-lc.in:
			return chunk, true
		default:
		}
		select {
		case chunk := <-lc.in:
			return chunk, true
		case <-lc.peerDone:
			// Frames that arrived before the close are still owed a reply.
			select {
			case chunk := <-lc.in:
				return chunk, true
			default:
				return nil, false
			}
		case <-s.ctx.Done():
			return nil, false
		}
	}}
	_ = agentproto.Serve(requests, agent, true, func(reply []byte) bool {
		return s.send(upData(id, reply))
	})
	s.send(upClose(id, ""))
}

// data hands request bytes from the daemon to connection id.
func (s *session) data(id uint64, data []byte) {
	lc := s.lookup(id)
	if lc == nil || len(data) == 0 {
		return
	}
	if len(data) > maxFrameBytes {
		lc.abort()
		return
	}
	select {
	case lc.in <- data:
	default:
		// The requests outran the agent by a whole queue: end this
		// connection rather than stall every other one on the stream.
		lc.abort()
	}
}

// peerClosed records the daemon's close of connection id: requests already
// received are still answered, then the connection ends.
func (s *session) peerClosed(id uint64) {
	if lc := s.lookup(id); lc != nil {
		lc.finishPeer()
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
		lc.abort()
	}
}
