package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// published_ports.go serves the published ports (`docker run -p`, a
// devcontainer's appPort) of instances whose backend leaves them to the
// daemon (backend.PortPublisher: Kubernetes): while an instance runs, one
// listener on the daemon host per port, each accepted connection dialled into
// the instance (backend.PortDialer) and relayed with its half-closes. A port
// listens on 127.0.0.1 unless the devcontainer names an address, unlike
// docker's 0.0.0.0 default: on a remote daemon a wildcard listener would put
// the instance on the daemon host's network with the cluster's credentials.
// What each listener is (its address, the port picked for a port the
// devcontainer left open, why it could not listen: another process holds the
// port) goes on the instance's InstanceRuntime.kube, where a TUI shows a
// conflict, and a wildcard address when its daemon is remote. A port that
// could not listen is tried again on a later pass.

// publishRetry is how soon a port that could not listen is tried again;
// publishDialTimeout bounds the dial into the instance of one connection.
const (
	publishRetry       = 30 * time.Second
	publishDialTimeout = 30 * time.Second
)

// portDial connects to a port in the instance.
type portDial func(ctx context.Context, port int) (io.ReadWriteCloser, error)

// portPublisher is the daemon's published-port listeners, by instance.
type portPublisher struct {
	retry time.Duration
	now   func() time.Time

	mu     sync.Mutex
	insts  map[string]*publishedInst
	closed bool
}

func newPortPublisher() *portPublisher {
	return &portPublisher{retry: publishRetry, now: time.Now, insts: map[string]*publishedInst{}}
}

// publishedInst is one instance's listeners. containerID names the
// container they forward to: another one (a rebuild) starts them over.
type publishedInst struct {
	label       string
	containerID string
	ports       map[backend.PublishedPort]*publishedPort
}

// publishedPort is one port's listener, or why it has none.
type publishedPort struct {
	spec     backend.PublishedPort
	ln       net.Listener
	port     int
	err      string
	failedAt time.Time
	dial     *portDialHolder
	conns    *publishedConns
}

// portDialHolder holds the dial a listener uses, so a pass can hand it a
// fresh handle without restarting it.
type portDialHolder struct {
	mu   sync.Mutex
	dial portDial
}

func (h *portDialHolder) get() portDial {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dial
}

func (h *portDialHolder) set(d portDial) {
	h.mu.Lock()
	h.dial = d
	h.mu.Unlock()
}

// publishedConns is a listener's live connections, closed with it. One
// added after that (a connection accepted, or dialled into the instance,
// while the listener closed) is closed at once.
type publishedConns struct {
	mu     sync.Mutex
	m      map[io.Closer]struct{}
	closed bool
}

func (s *publishedConns) add(c io.Closer) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.m[c] = struct{}{}
	s.mu.Unlock()
}

func (s *publishedConns) remove(c io.Closer) {
	s.mu.Lock()
	delete(s.m, c)
	s.mu.Unlock()
}

func (s *publishedConns) closeAll() {
	s.mu.Lock()
	all := make([]io.Closer, 0, len(s.m))
	for c := range s.m {
		all = append(all, c)
	}
	s.m = map[io.Closer]struct{}{}
	s.closed = true
	s.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

// sync makes key's listeners serve ports for containerID through dial, and
// returns what each one is. label names the instance in fleet.log.
func (pp *portPublisher) sync(key, label, containerID string, ports []backend.PublishedPort, dial portDial) []*fleetgrpc.KubePublishedPort {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.closed {
		return nil
	}
	inst := pp.insts[key]
	if inst != nil && inst.containerID != containerID {
		inst.closeLocked()
		inst = nil
	}
	if inst == nil {
		if len(ports) == 0 {
			delete(pp.insts, key)
			return nil
		}
		inst = &publishedInst{label: label, containerID: containerID, ports: map[backend.PublishedPort]*publishedPort{}}
		pp.insts[key] = inst
	}
	want := map[backend.PublishedPort]bool{}
	for _, spec := range ports {
		want[spec] = true
	}
	for spec, p := range inst.ports {
		if !want[spec] {
			p.close()
			delete(inst.ports, spec)
		}
	}
	now := pp.now()
	for spec := range want {
		p := inst.ports[spec]
		switch {
		case p == nil:
			p = &publishedPort{spec: spec, dial: &portDialHolder{dial: dial}, conns: &publishedConns{m: map[io.Closer]struct{}{}}}
			inst.ports[spec] = p
			p.listen(inst.label, now)
		case p.ln == nil && now.Sub(p.failedAt) >= pp.retry:
			p.dial.set(dial)
			p.listen(inst.label, now)
		default:
			p.dial.set(dial)
		}
	}
	if len(inst.ports) == 0 {
		delete(pp.insts, key)
	}
	return inst.status()
}

// drop closes key's listeners and their connections (the instance stopped,
// or is gone).
func (pp *portPublisher) drop(key string) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if inst := pp.insts[key]; inst != nil {
		inst.closeLocked()
		delete(pp.insts, key)
	}
}

// close closes every listener for good: the bounded shutdown's step.
func (pp *portPublisher) close() {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.closed = true
	for key, inst := range pp.insts {
		inst.closeLocked()
		delete(pp.insts, key)
	}
}

func (inst *publishedInst) closeLocked() {
	for _, p := range inst.ports {
		p.close()
	}
}

// status is what the instance's listeners are, in a stable order.
func (inst *publishedInst) status() []*fleetgrpc.KubePublishedPort {
	out := make([]*fleetgrpc.KubePublishedPort, 0, len(inst.ports))
	for _, p := range inst.ports {
		host := p.spec.HostPort
		if p.ln != nil {
			host = p.port
		}
		out = append(out, &fleetgrpc.KubePublishedPort{
			Address: publishHost(p.spec.HostIP), HostPort: int32(host), ContainerPort: int32(p.spec.ContainerPort), Error: p.err,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContainerPort != out[j].ContainerPort {
			return out[i].ContainerPort < out[j].ContainerPort
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// listen binds the port and starts serving it, or records why it could not.
func (p *publishedPort) listen(label string, now time.Time) {
	addr := net.JoinHostPort(publishHost(p.spec.HostIP), strconv.Itoa(p.spec.HostPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		why := listenError(err)
		if why != p.err {
			// Once per reason: the retries of a port another process
			// keeps are not news.
			flog.Warn("published port: cannot listen", "instance", label, "addr", addr, "containerPort", p.spec.ContainerPort, "err", err)
		}
		p.err, p.failedAt = why, now
		return
	}
	p.ln, p.err = ln, ""
	p.port = ln.Addr().(*net.TCPAddr).Port
	flog.Info("published port: listening", "instance", label, "addr", ln.Addr().String(), "containerPort", p.spec.ContainerPort)
	go p.serve(label, ln)
}

// publishHost is the address a port listens on: 127.0.0.1 when none is
// named.
func publishHost(ip string) string {
	if ip == "" {
		return "127.0.0.1"
	}
	return ip
}

// listenError is a listen failure in a row's words.
func listenError(err error) string {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return "the port is in use on the daemon host"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}

func (p *publishedPort) close() {
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	p.conns.closeAll()
}

// serve accepts connections until the listener closes.
func (p *publishedPort) serve(label string, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.relay(label, c)
	}
}

// relay connects one accepted connection into the instance and copies both
// ways, each EOF passed on as a half-close.
func (p *publishedPort) relay(label string, local net.Conn) {
	p.conns.add(local)
	defer p.conns.remove(local)
	defer local.Close()
	ctx, cancel := context.WithTimeout(context.Background(), publishDialTimeout)
	remote, err := p.dial.get()(ctx, p.spec.ContainerPort)
	cancel()
	if err != nil {
		flog.Warn("published port: dial into the instance", "instance", label, "containerPort", p.spec.ContainerPort, "err", err)
		return
	}
	p.conns.add(remote)
	defer p.conns.remove(remote)
	defer remote.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(local, remote)
		if cw, ok := local.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(remote, local)
	if cw, ok := remote.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = remote.Close()
	}
	<-done
}

// publishedLabel names an instance in the published-port log lines.
func publishedLabel(fleetName, instance string) string {
	return fmt.Sprintf("%s/%s", fleetName, instance)
}
