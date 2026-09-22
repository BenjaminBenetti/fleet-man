package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sshagent.go is the SERVER half of SSH-agent forwarding. Three parties meet
// here:
//
//   - PROVIDERS: clients (a TUI, a CLI command) holding an SSHAgent stream.
//     They own the real ssh-agent, on whatever machine the human sits at. The
//     most recently attached one is ACTIVE; the rest stand by and are promoted
//     if it leaves — superseding rather than rejecting, like the microphone.
//   - SOCKETS: the relay sockets this daemon listens on (sshagent_listen.go):
//     one on the host, which the daemon's own children use through
//     SSH_AUTH_SOCK, and one per devcontainer instance.
//   - the HUB below, which hands every connection made to a socket to the
//     newest provider that can serve it (a client whose machine has no agent
//     answers "cannot" and the next one is tried) — or, if none can, to the
//     agent the daemon itself was started with.
//
// The upstream is chosen per CONNECTION, never frozen at creation time, so a
// client reconnecting, a new client attaching, or the local agent restarting
// all take effect on the next `ssh`/`git` without touching any instance.

const (
	// agentReadyTimeout bounds the provider's answer to an open (dialing a
	// local unix socket is instant; this covers a slow remote link).
	agentReadyTimeout = 10 * time.Second
	// agentFallbackDialTimeout bounds dialing the daemon's own agent.
	agentFallbackDialTimeout = 3 * time.Second
	// agentReadChunk is how much of a connection is read per data frame.
	agentReadChunk = 32 * 1024
	// maxAgentFrameBytes bounds one data frame from a provider. OpenSSH caps
	// an agent message at 256 KiB, so anything near this is a broken client.
	maxAgentFrameBytes = 1 << 20
	// agentConnQueue bounds the provider→connection frames waiting for a
	// connection whose reader has stalled. Agent traffic is request/response,
	// so a full queue means the peer stopped reading: it is closed rather than
	// allowed to stall the provider's whole stream.
	agentConnQueue = 64
	// agentProviderQueue buffers frames waiting for the provider stream.
	agentProviderQueue = 64
	// maxAgentConnsPerProvider bounds concurrent connections on one provider;
	// beyond it new connections fall back as if there were no provider.
	maxAgentConnsPerProvider = 256
	// agentCloseGrace bounds how long a connection that has finished sending
	// (EOF, possibly only a half-close) waits for the provider to deliver the
	// replies it still owes and close its side.
	agentCloseGrace = 30 * time.Second
)

// agentPingInterval is how often a provider is probed; one that has not
// answered the previous probe by the next is SUSPECT and skipped until it
// answers anything. A var so tests can shorten it.
var agentPingInterval = 15 * time.Second

// agentHub owns providers, the relay sockets, and the routing between them.
type agentHub struct {
	mu        sync.Mutex
	providers []*agentProvider // attach order; the last is active
	// fallback is the agent the daemon was started with ("" for none).
	fallback string
	nextID   uint64

	// Listeners and live connections (sshagent_listen.go).
	host      *agentListener
	instances map[string]*instanceListener // "<fleet>/<instance>"
	opening   map[string]bool              // instance listens in flight
	retryAt   map[string]time.Time         // failed instance listens, retried after
	conns     map[net.Conn]struct{}
	closed    bool
	serving   sync.WaitGroup // connection goroutines
}

func newAgentHub() *agentHub {
	return &agentHub{
		instances: make(map[string]*instanceListener),
		opening:   make(map[string]bool),
		retryAt:   make(map[string]time.Time),
		conns:     make(map[net.Conn]struct{}),
	}
}

// setFallback records the agent the daemon was started with.
func (h *agentHub) setFallback(sock string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fallback = sock
}

func (h *agentHub) fallbackSock() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fallback
}

// providersNewestFirst snapshots the providers, active one first.
func (h *agentHub) providersNewestFirst() []*agentProvider {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := slices.Clone(h.providers)
	slices.Reverse(out)
	return out
}

func (h *agentHub) newConnID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return h.nextID
}

// addProvider attaches a stream as the new active provider; the previous
// active one is told it is standing by.
func (h *agentHub) addProvider(client string) *agentProvider {
	p := &agentProvider{
		client: client,
		out:    make(chan *fleetgrpc.SSHAgentDown, agentProviderQueue),
		status: make(chan bool, 1),
		done:   make(chan struct{}),
		conns:  make(map[uint64]*agentRelayConn),
	}
	h.mu.Lock()
	if n := len(h.providers); n > 0 {
		h.providers[n-1].postStatus(false)
	}
	h.providers = append(h.providers, p)
	p.postStatus(true)
	count := len(h.providers)
	h.mu.Unlock()
	flog.Info("ssh agent provider attached", "client", client, "providers", count)
	return p
}

// removeProvider detaches a provider, ends its connections, and promotes the
// newest remaining one.
func (h *agentHub) removeProvider(p *agentProvider) {
	h.mu.Lock()
	index := slices.Index(h.providers, p)
	if index >= 0 {
		wasActive := index == len(h.providers)-1
		h.providers = slices.Delete(h.providers, index, index+1)
		if n := len(h.providers); wasActive && n > 0 {
			h.providers[n-1].postStatus(true)
		}
	}
	count := len(h.providers)
	h.mu.Unlock()
	p.finish()
	flog.Info("ssh agent provider detached", "client", p.client, "providers", count)
}

// serveConn relays one connection made to a relay socket: to the active
// provider if it can serve it, else to the next-newest one that can (a client
// whose machine has no agent must not shadow one that does), else to the
// daemon's own agent, else it is simply closed (an agent client then carries
// on with the keys on disk).
func (h *agentHub) serveConn(conn net.Conn, origin string) {
	defer conn.Close()
	for _, p := range h.providersNewestFirst() {
		if p.suspect.Load() {
			// Not answering (a laptop asleep, a dead link TCP has not given
			// up on yet): trying it would stall this connection for
			// agentReadyTimeout, every time.
			continue
		}
		if p.relay(conn, origin, h.newConnID()) {
			return
		}
	}
	if sock := h.fallbackSock(); sock != "" {
		spliceAgent(conn, sock)
	}
}

// spliceAgent pipes conn to the agent at sock. A client that half-closes
// after its request still gets the reply: each direction's end is passed on
// as a half-close, and the splice ends once both are done (bounded).
func spliceAgent(conn net.Conn, sock string) {
	upstream, err := net.DialTimeout("unix", sock, agentFallbackDialTimeout)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(upstream, conn)
	go pipe(conn, upstream)
	<-done
	grace := time.NewTimer(agentCloseGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
	}
}

// agentProvider is one attached provider stream.
type agentProvider struct {
	client string
	// out carries frames to the provider; the RPC's send loop drains it (a
	// grpc stream's Send is not safe for concurrent use).
	out chan *fleetgrpc.SSHAgentDown
	// status is a one-slot, latest-wins mailbox for active/standby changes.
	status chan bool
	// done closes when the stream ends.
	done     chan struct{}
	doneOnce sync.Once

	mu    sync.Mutex
	conns map[uint64]*agentRelayConn

	// suspect is set when the provider stops answering (a missed ping, a
	// ready that never came) and cleared by any frame from it.
	suspect atomic.Bool
	// pingSent / pingAcked are the last probe sent and the last answered.
	pingSent  atomic.Uint64
	pingAcked atomic.Uint64
}

// postStatus replaces any unsent status with active.
func (p *agentProvider) postStatus(active bool) {
	for {
		select {
		case p.status <- active:
			return
		default:
		}
		select {
		case <-p.status:
		default:
		}
	}
}

// send queues a frame for the provider; false once the stream has ended.
func (p *agentProvider) send(msg *fleetgrpc.SSHAgentDown) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.out <- msg:
		return true
	case <-p.done:
		return false
	}
}

// finish ends the provider: every relayed connection is shut.
func (p *agentProvider) finish() {
	p.doneOnce.Do(func() {
		close(p.done)
		p.mu.Lock()
		conns := p.conns
		p.conns = make(map[uint64]*agentRelayConn)
		p.mu.Unlock()
		for _, rc := range conns {
			rc.shut()
		}
	})
}

func (p *agentProvider) register(rc *agentRelayConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return false
	default:
	}
	if len(p.conns) >= maxAgentConnsPerProvider {
		return false
	}
	p.conns[rc.id] = rc
	return true
}

func (p *agentProvider) unregister(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, id)
}

func (p *agentProvider) lookup(id uint64) *agentRelayConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[id]
}

// agentRelayConn is one connection being relayed through a provider.
type agentRelayConn struct {
	id uint64
	// ready gets the provider's answer to the open: "" once it dialed its
	// agent, otherwise why it could not.
	ready chan string
	// in carries bytes from the provider to the connection.
	in chan []byte
	// closed closes when the provider ends the connection (or goes away).
	closed    chan struct{}
	closeOnce sync.Once
}

func (rc *agentRelayConn) shut() {
	rc.closeOnce.Do(func() { close(rc.closed) })
}

// answer delivers the provider's answer to the open, if none arrived yet.
func (rc *agentRelayConn) answer(errMsg string) {
	select {
	case rc.ready <- errMsg:
	default:
	}
}

// relay serves conn through the provider. served=false means the provider
// could not take it (no agent on its machine, gone, saturated, too slow to
// answer) and nothing was exchanged, so the caller may fall back.
func (p *agentProvider) relay(conn net.Conn, origin string, id uint64) (served bool) {
	rc := &agentRelayConn{
		id:     id,
		ready:  make(chan string, 1),
		in:     make(chan []byte, agentConnQueue),
		closed: make(chan struct{}),
	}
	if !p.register(rc) {
		return false
	}
	defer p.unregister(id)
	if !p.send(agentDownOpen(id, origin)) {
		return false
	}

	timer := time.NewTimer(agentReadyTimeout)
	defer timer.Stop()
	select {
	case errMsg := <-rc.ready:
		if errMsg != "" {
			return false
		}
	case <-timer.C:
		p.suspect.Store(true)
		p.send(agentDownClose(id))
		return false
	case <-p.done:
		return false
	}
	flog.Info("ssh agent used", "origin", agentOriginLabel(origin), "client", p.client)

	// provider → connection. Frames that arrived before a close are written
	// before the connection is closed: the provider sends a reply and then
	// ends the connection, and the reply must not be lost.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer conn.Close()
		for {
			select {
			case data := <-rc.in:
				if _, err := conn.Write(data); err != nil {
					return
				}
			case <-rc.closed:
				for {
					select {
					case data := <-rc.in:
						if _, err := conn.Write(data); err != nil {
							return
						}
					default:
						return
					}
				}
			case <-p.done:
				return
			}
		}
	}()

	// connection → provider. The connection ending its side is only a
	// half-close as far as the provider is concerned: it still owes replies
	// to what was sent, so the writer runs on until the provider closes too.
	buf := make([]byte, agentReadChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if !p.send(agentDownData(id, append([]byte(nil), buf[:n]...))) {
				break
			}
		}
		if err != nil {
			select {
			case <-rc.closed: // the provider ended it; it knows
			default:
				p.send(agentDownClose(id))
			}
			break
		}
	}
	grace := time.NewTimer(agentCloseGrace)
	select {
	case <-writerDone:
	case <-grace.C:
	}
	grace.Stop()
	rc.shut()
	_ = conn.Close()
	<-writerDone
	return true
}

// deliver routes one frame from the provider. An error ends the stream (the
// provider broke the contract).
func (p *agentProvider) deliver(up *fleetgrpc.SSHAgentUp) error {
	p.suspect.Store(false) // it is talking
	switch msg := up.GetMsg().(type) {
	case *fleetgrpc.SSHAgentUp_Pong:
		p.pingAcked.Store(msg.Pong.GetSeq())
	case *fleetgrpc.SSHAgentUp_Ready:
		if rc := p.lookup(msg.Ready.GetConnId()); rc != nil {
			rc.answer("")
		}
	case *fleetgrpc.SSHAgentUp_Data:
		data := msg.Data.GetData()
		if len(data) > maxAgentFrameBytes {
			return status.Errorf(codes.InvalidArgument, "agent frame of %d bytes exceeds %d", len(data), maxAgentFrameBytes)
		}
		rc := p.lookup(msg.Data.GetConnId())
		if rc == nil || len(data) == 0 {
			return nil // late data for a connection that already ended
		}
		select {
		case rc.in <- data:
		default:
			// The connection stopped reading: drop it rather than stall
			// every other connection on this stream.
			p.unregister(rc.id)
			rc.shut()
			p.send(agentDownClose(rc.id))
		}
	case *fleetgrpc.SSHAgentUp_Close:
		if rc := p.lookup(msg.Close.GetConnId()); rc != nil {
			errMsg := msg.Close.GetError()
			if errMsg == "" {
				errMsg = "closed before ready"
			}
			rc.answer(errMsg) // a close before ready means "cannot serve"
			rc.shut()
		}
	case *fleetgrpc.SSHAgentUp_Hello:
		return status.Error(codes.InvalidArgument, "duplicate SSHAgent hello")
	}
	return nil
}

func agentDownOpen(id uint64, origin string) *fleetgrpc.SSHAgentDown {
	return &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Open{Open: &fleetgrpc.SSHAgentOpen{ConnId: id, Origin: origin}}}
}

func agentDownData(id uint64, data []byte) *fleetgrpc.SSHAgentDown {
	return &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Data{Data: &fleetgrpc.SSHAgentData{ConnId: id, Data: data}}}
}

func agentDownClose(id uint64) *fleetgrpc.SSHAgentDown {
	return &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Close{Close: &fleetgrpc.SSHAgentClose{ConnId: id}}}
}

// agentOriginLabel names a connection's origin for the log.
func agentOriginLabel(origin string) string {
	if origin == "" {
		return "host"
	}
	return origin
}

// errAgentForwardingOff is the refusal a provider gets from a host with
// FLEET_SSH_AGENT_SOCK=off: the host's owner keeps a kill switch.
var errAgentForwardingOff = status.Error(codes.FailedPrecondition,
	fmt.Sprintf("SSH agent forwarding is turned off on this host (%s=off)", agentsock.EnvOverride))

// SSHAgent implements the agent-forwarding data plane. The first client frame
// must carry the hello; afterwards the server announces connections and both
// sides exchange their bytes (see exec.proto SSHAgentUp).
func (s *service) SSHAgent(stream fleetgrpc.FleetService_SSHAgentServer) error {
	if agentsock.CurrentMode() == agentsock.ModeOff {
		return errAgentForwardingOff
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first SSHAgent frame must carry hello")
	}

	provider := s.agent.addProvider(hello.GetClient())
	defer s.agent.removeProvider(provider)

	recvDone := make(chan error, 1)
	go func() {
		for {
			up, err := stream.Recv()
			if err == nil {
				err = provider.deliver(up)
			}
			if err != nil {
				recvDone <- err
				return
			}
		}
	}()

	pings := time.NewTicker(agentPingInterval)
	defer pings.Stop()
	for {
		select {
		case <-pings.C:
			if provider.pingAcked.Load() < provider.pingSent.Load() {
				provider.suspect.Store(true) // the last probe went unanswered
			}
			seq := provider.pingSent.Add(1)
			msg := &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Ping{Ping: &fleetgrpc.SSHAgentPing{Seq: seq}}}
			if err := stream.Send(msg); err != nil {
				return err
			}
		case msg := <-provider.out:
			if err := stream.Send(msg); err != nil {
				return err
			}
		case active := <-provider.status:
			msg := &fleetgrpc.SSHAgentDown{Msg: &fleetgrpc.SSHAgentDown_Status{Status: &fleetgrpc.SSHAgentStatus{Active: active}}}
			if err := stream.Send(msg); err != nil {
				return err
			}
		case err := <-recvDone:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
