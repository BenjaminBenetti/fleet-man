//go:build linux

package podagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/hashicorp/yamux"
)

// handshakeTimeout bounds the wait for fleetd's HelloAck.
const handshakeTimeout = 30 * time.Second

// agent is one agent session.
type agent struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc
	sess   *yamux.Session
	host   *execas.Host
	log    *Logger
	proto  int
	rpc    map[string]Method
	ctlW   *podwire.FrameWriter
	// ctl is the main agent's work in the control directory: the in-pod
	// sockets and files fleetd's DesiredState asks for (nil on the bulk
	// session).
	ctl *controlDir

	lastRecv atomic.Int64
	streams  sync.WaitGroup
	endOnce  sync.Once

	// spokes counts the share spokes this agent relays to (serveSyncCtl):
	// while one runs, the pod's share changes need no hints.
	spokes atomic.Int32
	// hinter is the main agent's watch of a multi-node share
	// (sharehint_linux.go).
	hintMu sync.Mutex
	hinter *shareHinter
}

// Serve is `pod-agent serve`: it serves one agent session over conn (the
// exec's stdio) until fleetd closes it, it goes silent for the silence
// deadline, a newer agent takes over, or ctx ends. A main-role agent first
// takes over as the pod's one main agent. When the session ends, commands
// that are not held are aborted, and Serve returns once they have ended
// (bounded); held commands stay with their shepherds.
func Serve(ctx context.Context, conn io.ReadWriteCloser, opts Options) error {
	o := opts.withDefaults()
	if !o.Role.Valid() {
		_ = conn.Close()
		return fmt.Errorf("unknown pod agent role %q", o.Role)
	}
	sess, err := yamux.Server(conn, podwire.SessionConfig(o.Role))
	if err != nil {
		_ = conn.Close()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	a := &agent{opts: o, ctx: ctx, cancel: cancel, sess: sess, log: newLogger(o.Stderr)}
	// Last: whatever is logged without the control stream (before a failed
	// handshake, or after the session) goes to stderr.
	defer a.log.detach()
	defer a.end()
	stop := context.AfterFunc(ctx, a.end)
	defer stop()

	takeInitOOMScore(o, a.log)
	a.host = loadHost(o, a.log)
	ctl, r, err := a.handshake()
	if err != nil {
		return err
	}
	a.log.attach(a.ctlW)
	a.rpc = a.methods()
	if o.Role == podwire.RoleMain {
		if err := prepareControlDir(o.ControlDir); err != nil {
			a.log.Warn("could not make the control directory root's alone", "dir", o.ControlDir, "err", err.Error())
		}
		a.ctl = newControlDir(o.ControlDir, a.log, a.dialSock, a.relayOwner)
	}
	if o.Role == podwire.RoleMain && !o.NoTakeover {
		lock, err := a.takeOver()
		if err != nil {
			a.log.Error("could not become the pod's agent", "err", err.Error())
			return err
		}
		defer lock.release()
	}
	a.touch()
	go a.readControl(ctl, r)
	go a.heartbeat()
	if a.ctl != nil {
		go a.ctl.run(a.ctx.Done(), o.ControlCheck)
	}
	mux := &podwire.Mux{Handlers: a.handlers(), Refused: func(tag podwire.Tag, reason string) {
		a.log.Warn("refused a stream from fleetd", "kind", tag.String(), "reason", reason)
	}}
	_ = mux.Serve(sess)
	a.end()
	a.stopShareHint()
	// Before the pid file's lock goes: the next agent binds the sockets.
	if a.ctl != nil {
		a.ctl.close()
	}
	a.waitStreams(execas.AbortGrace + 2*time.Second)
	return nil
}

// dialSock opens a relay stream to fleetd for an in-pod connection.
func (a *agent) dialSock(tag podwire.Tag, open podwire.SockOpen) (net.Conn, error) {
	return podwire.DialSock(a.sess, tag, open)
}

// relayOwner is whom the host-socket binds belong to: the devcontainer's
// user (the exec class's), unless the agent cannot switch users.
func (a *agent) relayOwner() (execas.Identity, bool, error) {
	if a.host.SingleUID {
		return execas.Identity{}, false, nil
	}
	id, err := a.host.Resolve(execas.ClassExec, "")
	return id, true, err
}

// applyDesired makes the control directory match ds and acks it.
func (a *agent) applyDesired(ds podwire.DesiredState) {
	ack := podwire.DesiredAck{Gen: ds.Gen}
	if a.ctl == nil {
		ack.Error = fmt.Sprintf("the %s agent keeps no control directory", a.opts.Role)
	} else {
		ack.Error = a.ctl.apply(ds)
		a.setShareHint(ds.ShareHint)
	}
	_ = a.ctlW.WriteJSON(podwire.FrameData, podwire.ControlMessage{Type: podwire.MsgDesiredAck, DesiredAck: &ack})
}

// RefusedError is fleetd refusing this agent.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "fleetd refused this pod agent: " + e.Reason }

// loadHost is the Host commands are planned against.
func loadHost(o Options, log *Logger) *execas.Host {
	if o.Host != nil {
		return o.Host
	}
	spec, err := execas.LoadSpec(o.SpecPath)
	if err != nil {
		log.Error("no exec spec; commands get the container's defaults", "path", o.SpecPath, "err", err.Error())
		spec = execas.Spec{}
	}
	host := execas.NewHost(spec, o.CacheDir)
	if len(o.Bundles) > 0 {
		candidates := o.Bundles
		host.Bundles = func() []execas.Bundle { return installedBundles(candidates) }
	}
	return host
}

// installedBundles is the bundles of candidates that are installed now (their
// bin directory exists).
func installedBundles(candidates []execas.Bundle) []execas.Bundle {
	var out []execas.Bundle
	for _, b := range candidates {
		if fi, err := os.Stat(b.Bin); err == nil && fi.IsDir() {
			out = append(out, b)
		}
	}
	return out
}

// handshake accepts the control stream, sends Hello and reads HelloAck.
func (a *agent) handshake() (*yamux.Stream, *podwire.FrameReader, error) {
	actx, cancel := context.WithTimeout(a.ctx, podwire.HeaderDeadline)
	defer cancel()
	st, err := a.sess.AcceptStreamWithContext(actx)
	if err != nil {
		return nil, nil, fmt.Errorf("no control stream from fleetd: %w", err)
	}
	_ = st.SetReadDeadline(time.Now().Add(podwire.HeaderDeadline))
	tag, raw, err := podwire.ReadPreamble(st)
	if err != nil {
		return nil, nil, fmt.Errorf("control stream: %w", err)
	}
	if tag != podwire.TagControl {
		return nil, nil, fmt.Errorf("fleetd's first stream is %s, not control", tag)
	}
	var ch podwire.ControlHeader
	if err := podwire.DecodeHeader(raw, &ch); err != nil {
		return nil, nil, err
	}
	lo, hi := a.opts.protoRange()
	if ch.ProtoMax > 0 && ch.ProtoMax < hi {
		hi = max(ch.ProtoMax, lo)
	}
	w := podwire.NewFrameWriter(st)
	hello := podwire.Hello{
		ProtoMin: lo, ProtoMax: hi, Role: a.opts.Role, Version: a.opts.Version, SHA: selfSHA(),
		UID: os.Geteuid(), Arch: runtime.GOARCH, PodUID: a.opts.PodUID, CanSetuid: !a.host.SingleUID, PID: os.Getpid(),
	}
	if err := w.WriteJSON(podwire.FrameData, podwire.ControlMessage{Type: podwire.MsgHello, Hello: &hello}); err != nil {
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}
	_ = st.SetReadDeadline(time.Now().Add(handshakeTimeout))
	r := podwire.NewFrameReader(st)
	for {
		f, err := r.Next()
		if err != nil {
			return nil, nil, fmt.Errorf("read hello ack: %w", err)
		}
		var m podwire.ControlMessage
		if f.Type != podwire.FrameData || podwire.DecodeFrame(f, &m) != nil || m.Type != podwire.MsgHelloAck || m.HelloAck == nil {
			continue
		}
		_ = st.SetReadDeadline(time.Time{})
		if !m.HelloAck.Accept {
			return nil, nil, &RefusedError{Reason: m.HelloAck.Reason}
		}
		a.proto = m.HelloAck.Proto
		a.ctlW = w
		return st, r, nil
	}
}

// readControl reads fleetd's control frames: heartbeats, DesiredStates and
// Close.
func (a *agent) readControl(ctl *yamux.Stream, r *podwire.FrameReader) {
	defer ctl.Close()
	for {
		f, err := r.Next()
		if err != nil {
			a.end()
			return
		}
		a.touch()
		var m podwire.ControlMessage
		if f.Type != podwire.FrameData || podwire.DecodeFrame(f, &m) != nil {
			continue
		}
		switch {
		case m.Type == podwire.MsgClose:
			a.end()
			return
		case m.Type == podwire.MsgDesired && m.Desired != nil:
			a.applyDesired(*m.Desired)
		}
	}
}

// heartbeat pings fleetd and ends a session it stopped answering.
func (a *agent) heartbeat() {
	t := time.NewTicker(a.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-a.sess.CloseChan():
			return
		case <-t.C:
			if time.Since(time.Unix(0, a.lastRecv.Load())) > a.opts.Silence {
				a.log.Warn("fleetd went silent; closing the session", "after", a.opts.Silence.String())
				a.end()
				return
			}
			_ = a.ctlW.Write(podwire.FramePing, nil)
		}
	}
}

func (a *agent) touch() { a.lastRecv.Store(time.Now().UnixNano()) }

// end ends the session.
func (a *agent) end() {
	a.endOnce.Do(func() {
		a.cancel()
		_ = a.sess.Close()
	})
}

// handlers are the streams fleetd may open.
func (a *agent) handlers() map[podwire.Tag]podwire.Handler {
	track := func(fn podwire.Handler) podwire.Handler {
		return func(s *podwire.Stream) {
			a.streams.Add(1)
			defer a.streams.Done()
			fn(s)
		}
	}
	h := map[podwire.Tag]podwire.Handler{
		podwire.TagRPC:       track(a.serveRPC),
		podwire.TagExec:      track(a.serveExec),
		podwire.TagFileWrite: track(a.serveFileWrite),
		podwire.TagFileRead:  track(a.serveFileRead),
		podwire.TagForward:   track(a.serveForward),
	}
	// The share's spoke: its control connection rides the agent session, its
	// content the bulk one (spoke_linux.go).
	if a.opts.Role == podwire.RoleMain {
		h[podwire.TagSyncCtl] = track(a.serveSyncCtl)
	} else {
		h[podwire.TagSyncBulk] = track(a.serveSyncBulk)
	}
	return h
}

// waitStreams waits, at most d, for the streams being served to end.
func (a *agent) waitStreams(d time.Duration) {
	done := make(chan struct{})
	go func() {
		a.streams.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// serveExec runs a command, or re-attaches a held one.
func (a *agent) serveExec(s *podwire.Stream) {
	open := func(r podwire.OpenReply) { _ = s.W.WriteJSON(podwire.FrameOpen, r) }
	var h podwire.ExecHeader
	if err := podwire.DecodeHeader(s.Header, &h); err != nil {
		s.Penalize()
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitFailed})
		return
	}
	if err := h.Validate(); err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitFailed})
		return
	}
	cl := relayClient{r: s.R, w: s.W, c: s.Conn, finIsAbort: func() bool { return !s.TransportLost() }}
	if h.Resume != "" {
		resumeHeld(a.opts, cl, h, 0)
		return
	}
	plan, err := a.host.Plan(a.ctx, h.Request)
	if err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitCode(err)})
		return
	}
	plan.Cgroup = userCgroup(a.opts)
	if h.Resumable {
		startHeld(a.opts, a.log, cl, h, plan, 0)
		return
	}
	p, err := startProc(plan, false)
	if err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitCode(err)})
		return
	}
	p.protocolBreaker = s.Penalize
	if err := s.W.WriteJSON(podwire.FrameOpen, podwire.OpenReply{PID: p.pid()}); err != nil {
		p.giveUp()
		<-p.finishedCh
		return
	}
	att := &attachment{r: s.R, w: s.W, c: s.Conn, finIsAbort: cl.finIsAbort}
	if err := p.attach(att, 0, 0, nil); err != nil {
		p.giveUp()
	}
	<-p.finishedCh
}

// startHeld starts plan's command under a shepherd and relays cl to it.
func startHeld(o Options, log *Logger, cl relayClient, h podwire.ExecHeader, plan *execas.Plan, proto int) {
	open := func(r podwire.OpenReply) { _ = cl.w.WriteJSON(podwire.FrameOpen, r) }
	heldDir, err := privateDir(o.RunDir, heldDirName)
	if err != nil {
		log.Error("cannot hold commands", "err", err.Error())
		open(podwire.OpenReply{Error: "cannot hold commands: " + err.Error(), Code: execas.ExitFailed})
		return
	}
	id := newResumeID()
	rep, err := spawnShepherd(o, shepherdConfig{
		ID: id, Plan: plan, Resumable: h.Resumable, Class: h.Class, User: h.User, PodUID: o.PodUID,
		Socket: heldSocket(heldDir, id), HeldDir: heldDir, Grace: o.HoldGrace, MaxHeld: o.MaxHeld,
	})
	if err != nil {
		log.Error("could not start a shepherd", "err", err.Error())
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitFailed})
		return
	}
	if rep.Error != "" {
		code := rep.Code
		if code == 0 {
			code = execas.ExitFailed
		}
		open(podwire.OpenReply{Error: rep.Error, Code: code})
		return
	}
	attach := podwire.ExecHeader{Request: execas.Request{Class: h.Class, User: h.User}, Resume: id, PodUID: o.PodUID}
	sc, sr, first, err := dialShepherd(heldDir, attach)
	if err != nil || first.Type != podwire.FrameResume {
		if err == nil {
			err = fmt.Errorf("shepherd answered %s", first.Type)
			_ = sc.Close()
		}
		log.Error("could not reach a shepherd", "err", err.Error())
		open(podwire.OpenReply{Error: "reach the command's shepherd: " + err.Error(), Code: execas.ExitFailed})
		return
	}
	defer sc.Close()
	reply := podwire.OpenReply{PID: rep.PID, Proto: proto}
	if h.Resumable {
		reply.ResumeID = id
	}
	if err := cl.w.WriteJSON(podwire.FrameOpen, reply); err != nil {
		return
	}
	relay(cl, sc, sr)
}

// resumeHeld re-attaches cl to the held command h names.
func resumeHeld(o Options, cl relayClient, h podwire.ExecHeader, proto int) {
	gone := func() { _ = cl.w.WriteJSON(podwire.FrameOpen, podwire.OpenReply{Gone: true, Proto: proto}) }
	if o.PodUID != "" && h.PodUID != "" && h.PodUID != o.PodUID {
		gone()
		return
	}
	heldDir, err := privateDir(o.RunDir, heldDirName)
	if err != nil {
		gone()
		return
	}
	sc, sr, first, err := dialShepherd(heldDir, h)
	if err != nil {
		gone()
		return
	}
	defer sc.Close()
	if first.Type == podwire.FrameResume && proto > 0 {
		var rr podwire.ResumeReply
		if podwire.DecodeFrame(first, &rr) == nil {
			rr.Proto = proto
			if err := cl.w.WriteJSON(podwire.FrameResume, rr); err != nil {
				return
			}
			relay(cl, sc, sr)
			return
		}
	}
	if err := cl.w.Write(first.Type, first.Payload); err != nil || first.Type != podwire.FrameResume {
		return
	}
	relay(cl, sc, sr)
}

// errNoHeader is a direct exec whose client sent no preamble in time.
var errNoHeader = errors.New("no exec header on stdin")
