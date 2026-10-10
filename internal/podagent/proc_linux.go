//go:build linux

package podagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"golang.org/x/sys/unix"
)

// exitDrain bounds how long a command's output is still read after it exits:
// a background job that keeps the pipe open does not hold the stream, as it
// does not hold `docker exec`.
var exitDrain = 500 * time.Millisecond

// proc is one running command and its output, served to one attachment (an
// exec stream) at a time. A resumable proc keeps its output until the client
// acknowledges it and outlives its attachment; any other proc is aborted when
// its attachment goes away.
type proc struct {
	p         *execas.Process
	tty       bool
	resumable bool

	// stdinW feeds the command (the PTY master with a TTY); outR and errR
	// are its output (outR is the PTY master with a TTY, errR then nil).
	stdinW     *os.File
	outR, errR *os.File

	mu   sync.Mutex
	cond *sync.Cond
	// out and errOut are output not yet acknowledged (resumable) or not yet
	// sent (otherwise).
	out, errOut podwire.Ring
	outDone     bool
	errDone     bool
	exited      bool
	status      podwire.ExitStatus
	aborted     bool
	dropOutput  bool
	inQueue     []byte
	// inEOF: the client half-closed stdin; inClosed: the command's stdin is
	// gone (it closed it, or ended), so what still arrives is dropped.
	inEOF         bool
	inClosed      bool
	inRecv, inOff int64
	inAcked       int64
	att           *attachment
	// attached: an attachment has served the proc. Before the first one
	// (the agent writing OPEN, or a shepherd waiting for its agent) a proc
	// with none is not orphaned: its status is still to be sent.
	attached        bool
	finished        bool
	finishedCh      chan struct{}
	onDetach        func()
	onAttach        func()
	protocolBreaker func()
}

// attachment is one stream serving a proc.
type attachment struct {
	r *podwire.FrameReader
	w *podwire.FrameWriter
	c io.Closer
	// finIsAbort tells, once a read ends with io.EOF, whether that was the
	// client's FIN (abort) rather than a lost transport (hold).
	finIsAbort func() bool

	sentOut, sentErr int64
	exitSent         bool
	gone             bool
}

// startProc starts plan's command.
func startProc(plan *execas.Plan, resumable bool) (*proc, error) {
	p := &proc{tty: plan.TTY, resumable: resumable, finishedCh: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	if plan.TTY {
		process, err := execas.Start(plan, execas.Stdio{})
		if err != nil {
			return nil, err
		}
		p.p = process
		p.stdinW, p.outR = process.PTY(), process.PTY()
		p.errDone = true
	} else {
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, &execas.ExitError{Code: execas.ExitFailed, Err: err}
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			closeAll(inR, inW)
			return nil, &execas.ExitError{Code: execas.ExitFailed, Err: err}
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			closeAll(inR, inW, outR, outW)
			return nil, &execas.ExitError{Code: execas.ExitFailed, Err: err}
		}
		process, err := execas.Start(plan, execas.Stdio{Stdin: inR, Stdout: outW, Stderr: errW})
		closeAll(inR, outW, errW)
		if err != nil {
			closeAll(inW, outR, errR)
			return nil, err
		}
		p.p = process
		p.stdinW, p.outR, p.errR = inW, outR, errR
		go p.pump(errR, &p.errOut, &p.errDone)
	}
	go p.pump(p.outR, &p.out, &p.outDone)
	go p.feedStdin()
	go p.wait()
	return p, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// pump reads one output into its ring, waiting while the ring holds
// HoldBuffer (so the command's writes block, as a full pipe blocks them).
func (p *proc) pump(r *os.File, ring *podwire.Ring, done *bool) {
	buf := make([]byte, podwire.DataChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			p.mu.Lock()
			for ring.Len() >= podwire.HoldBuffer && !p.dropOutput {
				p.cond.Wait()
			}
			if !p.dropOutput {
				ring.Append(buf[:n])
			}
			p.cond.Broadcast()
			p.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	p.mu.Lock()
	*done = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// feedStdin writes what the client sent to the command, in its own goroutine
// so a command that does not read never stalls the stream's other frames.
func (p *proc) feedStdin() {
	defer func() {
		if !p.tty {
			_ = p.stdinW.Close()
		}
	}()
	for {
		p.mu.Lock()
		for len(p.inQueue) == 0 && !p.inEOF && !p.exited && !p.aborted {
			p.cond.Wait()
		}
		if p.exited || p.aborted || (len(p.inQueue) == 0 && p.inEOF) {
			p.inOff += int64(len(p.inQueue))
			p.inQueue = nil
			p.inClosed = true
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		chunk := p.inQueue[:min(len(p.inQueue), podwire.DataChunk)]
		p.mu.Unlock()
		n, err := p.stdinW.Write(chunk)
		p.mu.Lock()
		p.inQueue = p.inQueue[n:]
		p.inOff += int64(n)
		if err != nil {
			// The command closed its stdin: what it never reads counts as
			// taken, so the client's window still moves.
			p.inOff += int64(len(p.inQueue))
			p.inQueue = nil
			p.inClosed = true
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		p.cond.Broadcast()
		p.mu.Unlock()
	}
}

// wait reaps the command, drains its output for at most exitDrain, and
// records how it ended.
func (p *proc) wait() {
	st, err := p.p.Wait()
	drained := make(chan struct{})
	go func() {
		p.mu.Lock()
		for !p.outDone || !p.errDone {
			p.cond.Wait()
		}
		p.mu.Unlock()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(exitDrain):
		// Something else holds the output open: stop reading it.
		_ = p.outR.Close()
		if p.errR != nil {
			_ = p.errR.Close()
		}
		<-drained
	}
	_ = p.outR.Close()
	if p.errR != nil {
		_ = p.errR.Close()
	}
	p.mu.Lock()
	p.exited = true
	p.status = exitStatus(st, err)
	orphaned := p.att == nil && p.attached && (!p.resumable || p.aborted)
	p.cond.Broadcast()
	p.mu.Unlock()
	if orphaned {
		p.finish()
	}
}

// exitStatus is how a command ended, on the wire.
func exitStatus(st execas.ExitStatus, err error) podwire.ExitStatus {
	if st.Signal != 0 {
		return podwire.ExitStatus{Code: -1, Signal: signalName(st.Signal)}
	}
	if st.Code < 0 && err != nil {
		return podwire.ExitStatus{Code: -1, Error: err.Error()}
	}
	return podwire.ExitStatus{Code: st.Code}
}

// signalName is a signal's name without "SIG".
func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return strings.TrimPrefix(name, "SIG")
	}
	return fmt.Sprint(int(sig))
}

// signalByName is the signal a SIGNAL frame names, from podwire.Signals.
func signalByName(name string) (syscall.Signal, bool) {
	name = strings.TrimPrefix(strings.ToUpper(name), "SIG")
	for _, allowed := range podwire.Signals {
		if name == allowed {
			sig := unix.SignalNum("SIG" + name)
			return sig, sig != 0
		}
	}
	return 0, false
}

// attach makes att the proc's attachment, replacing any older one. from Out
// and fromErr are the offsets the client has (0 for a new stream); hello, when
// set, is written first (the re-attach's RESUME, made under the proc's lock
// so its stdin offset is exact).
func (p *proc) attach(att *attachment, fromOut, fromErr int64, hello func(stdin int64) any) error {
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return errors.New("the command has ended")
	}
	if _, ok := p.out.From(fromOut); !ok {
		p.mu.Unlock()
		return fmt.Errorf("stdout offset %d is not within %d-%d", fromOut, p.out.Base(), p.out.End())
	}
	if _, ok := p.errOut.From(fromErr); !ok {
		p.mu.Unlock()
		return fmt.Errorf("stderr offset %d is not within %d-%d", fromErr, p.errOut.Base(), p.errOut.End())
	}
	old := p.att
	if old != nil {
		old.gone = true
	}
	// A new attachment hears where stdin stands at once: an acknowledgement
	// lost with the old transport would otherwise leave a client waiting on
	// a full window.
	p.inAcked = -1
	if p.resumable {
		p.out.Trim(fromOut)
		p.errOut.Trim(fromErr)
	}
	att.sentOut, att.sentErr = fromOut, fromErr
	p.att = att
	p.attached = true
	var first any
	if hello != nil {
		first = hello(p.inRecv)
	}
	if p.onAttach != nil {
		p.onAttach()
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	if old != nil {
		_ = old.c.Close()
	}
	if first != nil {
		if err := att.w.WriteJSON(podwire.FrameResume, first); err != nil {
			p.lost(att)
			return err
		}
	}
	go p.send(att)
	go p.read(att)
	return nil
}

// send writes the proc's output, stdin acknowledgements and, at the end, its
// status to att.
func (p *proc) send(att *attachment) {
	for {
		p.mu.Lock()
		for !att.gone && !p.hasWork(att) {
			p.cond.Wait()
		}
		if att.gone {
			p.mu.Unlock()
			return
		}
		out := chunkFrom(&p.out, att.sentOut)
		errOut := chunkFrom(&p.errOut, att.sentErr)
		var ack *podwire.Ping
		if p.ackDue() {
			p.inAcked = p.inOff
			ack = &podwire.Ping{Stdin: p.inOff}
		}
		sendExit := len(out) == 0 && len(errOut) == 0 && p.exitReady(att)
		if sendExit {
			// Before it goes out: the client's answering EXIT may come
			// back before this goroutine runs again, and must count.
			att.exitSent = true
		}
		st := p.status
		p.mu.Unlock()

		var err error
		if len(out) > 0 {
			err = att.w.Write(podwire.FrameData, out)
		}
		if err == nil && len(errOut) > 0 {
			err = att.w.Write(podwire.FrameStderr, errOut)
		}
		if err == nil && ack != nil {
			err = att.w.WriteJSON(podwire.FramePing, *ack)
		}
		if err == nil && sendExit {
			err = att.w.WriteJSON(podwire.FrameExit, st)
		}
		if err != nil {
			p.lost(att)
			return
		}

		p.mu.Lock()
		att.sentOut += int64(len(out))
		att.sentErr += int64(len(errOut))
		if !p.resumable {
			p.out.Trim(att.sentOut)
			p.errOut.Trim(att.sentErr)
		}
		p.cond.Broadcast()
		p.mu.Unlock()
		if sendExit && !p.resumable {
			// Nothing to wait for: the stream is done.
			p.finish()
			return
		}
	}
}

// chunkFrom copies up to one frame of ring's bytes from off.
func chunkFrom(ring *podwire.Ring, off int64) []byte {
	data, ok := ring.From(off)
	if !ok || len(data) == 0 {
		return nil
	}
	return append([]byte(nil), data[:min(len(data), podwire.DataChunk)]...)
}

// hasWork reports whether att has anything to send. Lock held.
func (p *proc) hasWork(att *attachment) bool {
	return att.sentOut < p.out.End() || att.sentErr < p.errOut.End() || p.ackDue() || p.exitReady(att)
}

// ackDue reports whether the client should hear how much input the command
// consumed: every AckEvery bytes, and once on each new attachment. Lock held.
func (p *proc) ackDue() bool {
	return (p.inAcked < 0 && p.inOff > 0) || p.inOff-p.inAcked >= podwire.AckEvery
}

// exitReady reports whether the status is due on att: the command ended and
// every byte of its output went out. Lock held.
func (p *proc) exitReady(att *attachment) bool {
	return p.exited && p.outDone && p.errDone && !att.exitSent &&
		att.sentOut == p.out.End() && att.sentErr == p.errOut.End()
}

// read takes the client's frames on att.
func (p *proc) read(att *attachment) {
	for {
		f, err := att.r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) && att.finIsAbort != nil && att.finIsAbort() {
				// The client's FIN before EXIT: abort, and still send the
				// status down the half it keeps open.
				p.abort()
				return
			}
			p.lost(att)
			return
		}
		switch f.Type {
		case podwire.FrameData:
			p.mu.Lock()
			if len(p.inQueue)+len(f.Payload) > podwire.HoldBuffer+podwire.MaxFrame || p.inEOF {
				// Past the window the client keeps, or after EOF: a broken
				// client.
				p.mu.Unlock()
				if p.protocolBreaker != nil {
					p.protocolBreaker()
				}
				p.abort()
				continue
			}
			if p.inClosed {
				p.inOff += int64(len(f.Payload))
			} else {
				p.inQueue = append(p.inQueue, f.Payload...)
			}
			p.inRecv += int64(len(f.Payload))
			p.cond.Broadcast()
			p.mu.Unlock()
		case podwire.FrameEOF:
			p.mu.Lock()
			p.inEOF = true
			p.cond.Broadcast()
			p.mu.Unlock()
		case podwire.FrameResize:
			var rs podwire.Resize
			if podwire.DecodeFrame(f, &rs) == nil {
				_ = p.p.Resize(rs.Rows, rs.Cols)
			}
		case podwire.FrameSignal:
			var sg podwire.Signal
			if podwire.DecodeFrame(f, &sg) == nil {
				if sig, ok := signalByName(sg.Signal); ok {
					_ = p.p.Signal(sig)
				}
			}
		case podwire.FrameAbort:
			p.abort()
		case podwire.FramePing:
			var ping podwire.Ping
			if p.resumable && podwire.DecodeFrame(f, &ping) == nil {
				p.mu.Lock()
				p.out.Trim(min(ping.Stdout, att.sentOut))
				p.errOut.Trim(min(ping.Stderr, att.sentErr))
				p.cond.Broadcast()
				p.mu.Unlock()
			}
		case podwire.FrameExit:
			// A resumable client has the status: done.
			p.mu.Lock()
			sent := att.exitSent
			p.mu.Unlock()
			if sent {
				p.finish()
				_ = att.c.Close()
				return
			}
		}
	}
}

// lost ends att without the client having the status: a resumable proc that
// is not being aborted waits for another attachment, any other is aborted.
func (p *proc) lost(att *attachment) {
	p.mu.Lock()
	if att.gone {
		p.mu.Unlock()
		return
	}
	att.gone = true
	p.att = nil
	defer func() { _ = att.c.Close() }()
	if p.finished {
		p.mu.Unlock()
		return
	}
	if p.resumable && !p.aborted {
		hook := p.onDetach
		p.cond.Broadcast()
		p.mu.Unlock()
		if hook != nil {
			hook()
		}
		return
	}
	p.dropOutput = true
	exited := p.exited
	p.cond.Broadcast()
	p.mu.Unlock()
	if exited {
		p.finish()
		return
	}
	p.abort()
}

// abort ends the command: its TTY hung up or its process group sent
// SIGTERM, then everything left in its session killed after
// execas.AbortGrace.
func (p *proc) abort() {
	p.mu.Lock()
	if p.aborted || p.exited {
		p.mu.Unlock()
		return
	}
	p.aborted = true
	p.cond.Broadcast()
	p.mu.Unlock()
	p.p.Abort()
}

// giveUp ends a held proc nobody will re-attach: aborted, its output
// dropped, finished once it exits.
func (p *proc) giveUp() {
	p.mu.Lock()
	p.dropOutput = true
	exited := p.exited && p.att == nil
	p.cond.Broadcast()
	p.mu.Unlock()
	p.abort()
	p.mu.Lock()
	p.aborted = true
	p.mu.Unlock()
	if exited {
		p.finish()
	}
	go func() {
		p.mu.Lock()
		for !p.exited {
			p.cond.Wait()
		}
		p.mu.Unlock()
		p.finish()
	}()
}

// finish marks the proc done: its status reached the client, or nobody is
// left to take it.
func (p *proc) finish() {
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	p.finished = true
	p.dropOutput = true
	att := p.att
	p.att = nil
	if att != nil {
		att.gone = true
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	close(p.finishedCh)
}

// pid is the command's pid.
func (p *proc) pid() int { return p.p.Pid() }
