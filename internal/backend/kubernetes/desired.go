//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// desired.go is what fleetd wants in each instance pod's control directory,
// delivered on the pod's agent session as a podwire.DesiredState: which
// in-pod sockets the agent binds (the daemon's answer, SetDesiredSource), and
// the files the daemon keeps there (the handle's WriteControlFile: the status
// mod's). A new session gets the whole state, applied, before EnsureAgent
// hands it out, so the creation hooks already find the ssh-agent socket (as
// a docker instance finds the relay before its postCreate); a live one gets it
// again at once on every change. The state outlives sessions: a pod that
// restarted, or a session that reconnected, gets it again.
//
// The pool also routes the streams the agent opens (the in-pod sockets'
// relays) to the daemon's handler (SetInbound), tagged with the pod the
// session was verified and bootstrapped for: a relayed connection is that
// instance's, whatever the pod says.

// DesiredSource is the daemon's answer to which in-pod sockets the pod of t
// binds now. It is asked for every state a session sends, so it must be
// cheap.
type DesiredSource func(t PodTarget) podwire.Sockets

// InboundHandler serves one stream the agent of t's pod opened (a relay of
// one in-pod connection); the stream is closed when it returns.
type InboundHandler func(t PodTarget, s *podwire.Stream)

// InboundTags are the kinds of streams the agent opens that the pool hands
// the daemon's InboundHandler.
var InboundTags = podwire.SockTags

// desiredApplyWait bounds a new session's wait for its agent to apply the
// first state.
const desiredApplyWait = 15 * time.Second

// desiredEntry is the control files the daemon keeps in one instance's pod.
type desiredEntry struct {
	files map[string]podwire.ControlFile
	// version counts the changes: a session sends whenever it moved.
	version uint64
}

// desiredMark is what a session last sent: its entry's version and the
// pool's sockets epoch.
type desiredMark struct{ version, epoch uint64 }

// SetDesiredSource sets the daemon's answer to which in-pod sockets each pod
// binds, and has every live session send its state again.
func (p *Pool) SetDesiredSource(fn DesiredSource) {
	p.desiredMu.Lock()
	p.desiredSrc = fn
	p.desiredMu.Unlock()
	p.RefreshDesired()
}

// SetInbound sets the handler of the streams the agents open. Without one
// they are refused.
func (p *Pool) SetInbound(fn InboundHandler) {
	p.desiredMu.Lock()
	p.inbound = fn
	p.desiredMu.Unlock()
}

// RefreshDesired has every live session send its state again, asking the
// source anew: the daemon calls it when its answer may have changed (a TUI
// came or went, a fleet's Fleet MCP flipped, the agent relay's usability
// changed).
func (p *Pool) RefreshDesired() {
	p.desiredMu.Lock()
	p.socketsEpoch++
	p.desiredMu.Unlock()
	p.mu.Lock()
	slots := make([]*podSlot, 0, len(p.slots))
	for _, s := range p.slots {
		slots = append(slots, s)
	}
	p.mu.Unlock()
	for _, s := range slots {
		if sess := s.live(); sess != nil {
			sess.kickDesired()
		}
	}
}

// SetControlFile makes name hold data, with mode, in the control directory
// of the pod of the instance whose StatefulSet is sts on connection cluster:
// at once while the pod has an agent session, and in every session after.
// Every call is delivered, an unchanged file's too, which repairs a pod that
// lost it.
func (p *Pool) SetControlFile(cluster, sts, name string, data []byte, mode fs.FileMode) error {
	if !podwire.ValidControlFileName(name) {
		return fmt.Errorf("control file name %q is not allowed", name)
	}
	k := trackKey{cluster, sts}
	p.desiredMu.Lock()
	e := p.desired[k]
	if e == nil {
		e = &desiredEntry{files: map[string]podwire.ControlFile{}}
		p.desired[k] = e
	}
	prev, had := e.files[name]
	e.files[name] = podwire.ControlFile{Name: name, Mode: mode.Perm(), Data: bytes.Clone(data)}
	if err := (podwire.DesiredState{Files: e.list()}).Validate(); err != nil {
		if had {
			e.files[name] = prev
		} else {
			delete(e.files, name)
		}
		p.desiredMu.Unlock()
		return err
	}
	e.version++
	p.desiredMu.Unlock()
	p.kickDesired(k)
	return nil
}

// RemoveControlFile removes name from the pod's control directory (and from
// what later sessions get).
func (p *Pool) RemoveControlFile(cluster, sts, name string) {
	k := trackKey{cluster, sts}
	p.desiredMu.Lock()
	e := p.desired[k]
	if e == nil {
		p.desiredMu.Unlock()
		return
	}
	delete(e.files, name)
	e.version++
	p.desiredMu.Unlock()
	p.kickDesired(k)
}

// ControlFiles is what the pool keeps for the instance's pod, by name (for
// tests and diagnostics).
func (p *Pool) ControlFiles(cluster, sts string) map[string][]byte {
	p.desiredMu.Lock()
	defer p.desiredMu.Unlock()
	out := map[string][]byte{}
	if e := p.desired[trackKey{cluster, sts}]; e != nil {
		for name, f := range e.files {
			out[name] = bytes.Clone(f.Data)
		}
	}
	return out
}

// forgetDesired drops what the pool keeps for an instance (Forget).
func (p *Pool) forgetDesired(k trackKey) {
	p.desiredMu.Lock()
	delete(p.desired, k)
	p.desiredMu.Unlock()
}

// kickDesired has the live session of the instance's pod send its state.
func (p *Pool) kickDesired(k trackKey) {
	for _, s := range p.slotsOf(k) {
		if sess := s.live(); sess != nil {
			sess.kickDesired()
		}
	}
}

// list is the entry's files, by name.
func (e *desiredEntry) list() []podwire.ControlFile {
	out := make([]podwire.ControlFile, 0, len(e.files))
	for _, f := range e.files {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b podwire.ControlFile) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// desiredState is the state of t's pod now, numbered, and what it was made
// from.
func (p *Pool) desiredState(t PodTarget) (podwire.DesiredState, desiredMark) {
	p.desiredMu.Lock()
	src := p.desiredSrc
	mark := desiredMark{epoch: p.socketsEpoch}
	var files []podwire.ControlFile
	if e := p.desired[trackKey{t.Cluster, t.StatefulSet}]; e != nil {
		files, mark.version = e.list(), e.version
	}
	p.desiredGen++
	ds := podwire.DesiredState{Gen: p.desiredGen, Files: files}
	p.desiredMu.Unlock()
	if src != nil {
		ds.Sockets = src(t)
	}
	return ds, mark
}

// applyDesired sends a new session its first state and waits (within ctx and
// desiredApplyWait) for the agent to apply it. An agent of an older protocol
// gets none: it binds no sockets until it is restaged.
func (p *Pool) applyDesired(ctx context.Context, s *AgentSession) error {
	if !s.main.SupportsDesired() {
		return nil
	}
	ds, mark := p.desiredState(s.target)
	if err := s.main.SendDesired(ds); err != nil {
		return err
	}
	s.noteSent(ds, mark)
	wctx, cancel := context.WithTimeout(ctx, desiredApplyWait)
	defer cancel()
	ack, err := s.main.WaitDesired(wctx, ds.Gen)
	if err != nil {
		return err
	}
	if ack.Error != "" {
		// The agent logged what failed itself (fleet.log, at error).
		return fmt.Errorf("the pod agent applied the control directory only in part: %s", ack.Error)
	}
	return nil
}

// kickDesired asks the session's sender for a send.
func (s *AgentSession) kickDesired() {
	select {
	case s.desiredKick <- struct{}{}:
	default:
	}
}

// sendDesired sends the session's state whenever it changed, until the
// session ends. It sends once at its start for what changed between the
// bootstrap's send and the session being handed out.
func (s *AgentSession) sendDesired() {
	if !s.main.SupportsDesired() {
		return
	}
	s.kickDesired()
	for {
		select {
		case <-s.main.Done():
			return
		case <-s.desiredKick:
		}
		ds, mark := s.pool.desiredState(s.target)
		if mark == s.sentMarkNow() {
			continue
		}
		if err := s.main.SendDesired(ds); err != nil {
			if s.main.Err() == nil {
				s.pool.opts.Logger.Warn("kube agent: could not send the pod its desired state", "fleet", s.target.Fleet, "instance", s.target.Instance, "err", err)
			}
			continue
		}
		s.noteSent(ds, mark)
	}
}

func (s *AgentSession) noteSent(ds podwire.DesiredState, mark desiredMark) {
	s.desiredMu.Lock()
	s.sentMark, s.sentSockets = mark, ds.Sockets
	s.desiredMu.Unlock()
}

func (s *AgentSession) sentMarkNow() desiredMark {
	s.desiredMu.Lock()
	defer s.desiredMu.Unlock()
	return s.sentMark
}

// Sockets is the in-pod sockets the session last asked its agent to bind.
func (s *AgentSession) Sockets() podwire.Sockets {
	s.desiredMu.Lock()
	defer s.desiredMu.Unlock()
	return s.sentSockets
}

// Target is the pod the session serves, as the bootstrap that made it named
// it: the instance its relayed connections belong to.
func (s *AgentSession) Target() PodTarget { return s.target }

// mcpLiveWindow is how recently an in-pod MCP stream must have been open for
// the pod's session drop to count as one with a live MCP stream.
const mcpLiveWindow = 2 * time.Second

// inboundHandlers are the handlers of the streams the agent of t's pod opens
// on its main session: each goes to the daemon's InboundHandler (refused
// while there is none), and the pod's MCP streams are counted, so a session
// that drops with one live reconnects first.
func (p *Pool) inboundHandlers(slot *podSlot, t PodTarget) map[podwire.Tag]podwire.Handler {
	h := make(map[podwire.Tag]podwire.Handler, len(InboundTags))
	for _, tag := range InboundTags {
		h[tag] = func(st *podwire.Stream) {
			p.desiredMu.Lock()
			fn := p.inbound
			p.desiredMu.Unlock()
			if fn == nil {
				podwire.RefuseSock(st, podwire.RefusedUnsupported, "this fleetd serves no in-pod sockets")
				return
			}
			if tag == podwire.TagSockMCP {
				slot.mcpOpened()
				defer slot.mcpClosed()
			}
			fn(t, st)
		}
	}
	return h
}

func (s *podSlot) mcpOpened() {
	s.mu.Lock()
	s.mcpOpen++
	s.mcpLast = s.pool.opts.Now()
	s.mu.Unlock()
}

func (s *podSlot) mcpClosed() {
	s.mu.Lock()
	s.mcpOpen--
	s.mcpLast = s.pool.opts.Now()
	s.mu.Unlock()
}

// mcpLiveLocked reports whether an in-pod MCP stream is open, or was a
// moment ago (one the drop itself just ended). Called with s.mu held.
func (s *podSlot) mcpLiveLocked() bool {
	return s.mcpOpen > 0 || (!s.mcpLast.IsZero() && s.pool.opts.Now().Sub(s.mcpLast) < mcpLiveWindow)
}
