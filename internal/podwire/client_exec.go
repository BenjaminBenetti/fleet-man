package podwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// Errors an Exec ends with.
var (
	// ErrConnectionLost: the transport went away and the command was not
	// resumable (or was being aborted), so its status is unknown.
	ErrConnectionLost = errors.New("connection to the pod agent lost")
	// ErrHeldStreamGone: a re-attach found no held stream: the pod restarted,
	// its grace ran out, or it was ended to make room.
	ErrHeldStreamGone = errors.New("the held command is gone")
	// ErrStdinClosed: stdin was written after the command ended, the stream
	// was closed, or stdin was closed.
	ErrStdinClosed = errors.New("exec stdin is closed")
	// ErrReplyTooLarge: a buffered reply passed MaxReply.
	ErrReplyTooLarge = fmt.Errorf("reply is over %d bytes", MaxReply)
)

// StartError is a command the agent could not start, with the exit code
// `docker exec` gives for the same failure (125, 126, 127).
type StartError struct {
	Code    int
	Message string
}

func (e *StartError) Error() string { return e.Message }

// abortWait bounds how long Close waits for the agent's EXIT before it gives
// up on the transport: the agent kills what is left of an aborted command
// after execas.AbortGrace.
const abortWait = execas.AbortGrace + 5*time.Second

// ExecOptions configure OpenExec.
type ExecOptions struct {
	// Heartbeat sends a PING every StreamPingInterval and counts
	// StreamSilence without a frame as a lost transport: for a stream that
	// crosses the network on its own, not on a session that has its own.
	Heartbeat bool
}

// Exec is a command an agent runs. It is a backend.ExecStream, plus what a
// resumable command needs: its resume id, Detached and Reattach.
type Exec struct {
	header    ExecHeader
	opts      ExecOptions
	resumable bool
	id        string
	pid       int

	stdout, stderr *pipe
	stdin          execStdin

	// sendMu orders what goes out as stdin: a write's frames, and a
	// re-attach's resend of what was not acknowledged.
	sendMu sync.Mutex

	mu       sync.Mutex
	cond     *sync.Cond
	conn     io.ReadWriteCloser
	w        *FrameWriter
	gen      int
	lastRecv time.Time
	// Output received, and acknowledged so far (resumable).
	outOff, errOff int64
	ackOut, ackErr int64
	// in is stdin written but not yet taken by the agent (resumable); inEnd
	// is every byte of stdin written, and inAcked what the agent says the
	// command consumed: no stream ever has more than HoldBuffer in between,
	// the agent's input window.
	in      Ring
	inEnd   int64
	inAcked int64
	inEOF   bool
	size    *Resize
	exited  bool
	status  ExitStatus
	err     error
	closed  bool
	// detached is closed while a resumable command has no transport.
	detached chan struct{}
	done     chan struct{}
}

// Exec starts a command on the session.
func (c *Client) Exec(ctx context.Context, header ExecHeader) (*Exec, error) {
	if header.Resume != "" {
		return nil, errors.New("exec: use Reattach to re-attach a held command")
	}
	t, err := c.open(ctx, TagExec, header)
	if err != nil {
		return nil, err
	}
	e, err := startExec(ctx, t, header, ExecOptions{})
	if err != nil {
		_ = t.Close()
		return nil, err
	}
	return e, nil
}

// Reattach re-attaches a resumable command that lost its transport to this
// session (after a reconnect). Re-attaching a command that was closed or
// failed meanwhile aborts it in the pod.
func (c *Client) Reattach(ctx context.Context, e *Exec) error {
	t, err := c.open(ctx, TagExec, e.resumeHeader())
	if err != nil {
		return err
	}
	if err := e.finishReattach(ctx, t); err != nil {
		_ = t.Close()
		return err
	}
	return nil
}

// OpenExec starts a command over rw, a transport that carries this one exec
// stream (a direct-mode `pod-agent exec --stdio`'s stdio). header.Resume set
// re-attaches instead, with header's offsets; ReattachOver does that for an
// Exec already in hand. ctx bounds the start.
func OpenExec(ctx context.Context, rw io.ReadWriteCloser, header ExecHeader, opts ExecOptions) (*Exec, error) {
	if header.ProtoMin == 0 && header.ProtoMax == 0 {
		header.ProtoMin, header.ProtoMax = ProtoMin, ProtoMax
	}
	if err := WritePreamble(rw, TagExec, header); err != nil {
		return nil, err
	}
	if header.Resume != "" {
		e := newExec(header, opts, OpenReply{ResumeID: header.Resume})
		e.outOff, e.errOff = header.Stdout, header.Stderr
		e.ackOut, e.ackErr = header.Stdout, header.Stderr
		if err := e.finishReattach(ctx, rw); err != nil {
			return nil, err
		}
		return e, nil
	}
	return startExec(ctx, rw, header, opts)
}

// ReattachOver re-attaches e over rw, a fresh single-stream transport.
func (e *Exec) ReattachOver(ctx context.Context, rw io.ReadWriteCloser) error {
	header := e.resumeHeader()
	header.ProtoMin, header.ProtoMax = ProtoMin, ProtoMax
	if err := WritePreamble(rw, TagExec, header); err != nil {
		return err
	}
	return e.finishReattach(ctx, rw)
}

// startExec reads the agent's OPEN on a stream whose preamble is written, and
// attaches.
func startExec(ctx context.Context, rw io.ReadWriteCloser, header ExecHeader, opts ExecOptions) (*Exec, error) {
	r := NewFrameReader(rw)
	f, err := firstFrame(ctx, rw, r)
	if err != nil {
		return nil, fmt.Errorf("start exec: %w", err)
	}
	if f.Type != FrameOpen {
		return nil, fmt.Errorf("start exec: unexpected %s frame", f.Type)
	}
	var reply OpenReply
	if err := DecodeFrame(f, &reply); err != nil {
		return nil, err
	}
	if reply.Refused != "" {
		return nil, fmt.Errorf("%w: %s", ErrRefused, reply.Error)
	}
	if reply.Error != "" {
		code := reply.Code
		if code == 0 {
			code = execas.ExitFailed
		}
		return nil, &StartError{Code: code, Message: reply.Error}
	}
	if header.Resumable && !ValidResumeID(reply.ResumeID) {
		return nil, errors.New("start exec: the agent gave no resume id")
	}
	e := newExec(header, opts, reply)
	if err := e.attach(rw, r, nil); err != nil {
		return nil, err
	}
	return e, nil
}

func newExec(header ExecHeader, opts ExecOptions, reply OpenReply) *Exec {
	e := &Exec{
		header:    header,
		opts:      opts,
		resumable: header.Resumable || header.Resume != "",
		id:        reply.ResumeID,
		pid:       reply.PID,
		stdout:    newPipe(pipeBuffer),
		stderr:    newPipe(pipeBuffer),
		detached:  make(chan struct{}),
		done:      make(chan struct{}),
	}
	e.cond = sync.NewCond(&e.mu)
	e.stdin = execStdin{e}
	if !header.TTY {
		return e
	}
	e.size = &Resize{Rows: header.Rows, Cols: header.Cols}
	return e
}

// resumeHeader is the preamble header that re-attaches e.
func (e *Exec) resumeHeader() ExecHeader {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ExecHeader{
		Request: execas.Request{Class: e.header.Class, User: e.header.User},
		Resume:  e.id,
		PodUID:  e.header.PodUID,
		Stdout:  e.outOff,
		Stderr:  e.errOff,
	}
}

// finishReattach reads the agent's answer to a re-attach and attaches.
func (e *Exec) finishReattach(ctx context.Context, rw io.ReadWriteCloser) error {
	if !e.resumable || e.id == "" {
		return errors.New("re-attach: the command is not resumable")
	}
	r := NewFrameReader(rw)
	f, err := firstFrame(ctx, rw, r)
	if err != nil {
		return fmt.Errorf("re-attach: %w", err)
	}
	switch f.Type {
	case FrameResume:
		var reply ResumeReply
		if err := DecodeFrame(f, &reply); err != nil {
			return err
		}
		e.mu.Lock()
		ok := reply.Stdout == e.outOff && reply.Stderr == e.errOff
		e.mu.Unlock()
		if !ok {
			return errors.New("re-attach: the agent replays from other offsets")
		}
		if reply.PID != 0 {
			e.pid = reply.PID
		}
		return e.attach(rw, r, &reply)
	case FrameOpen:
		var reply OpenReply
		if err := DecodeFrame(f, &reply); err != nil {
			return err
		}
		if reply.Gone {
			e.finish(ExitStatus{Code: -1}, ErrHeldStreamGone)
			return ErrHeldStreamGone
		}
		if reply.Refused != "" {
			return fmt.Errorf("%w: %s", ErrRefused, reply.Error)
		}
		return fmt.Errorf("re-attach: %s", reply.Error)
	}
	return fmt.Errorf("re-attach: unexpected %s frame", f.Type)
}

// attach makes rw the command's transport. On a re-attach it first resends
// the input the agent has not taken, then the pending EOF, size and abort.
func (e *Exec) attach(rw io.ReadWriteCloser, r *FrameReader, resumed *ResumeReply) error {
	w := NewFrameWriter(rw)
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	e.mu.Lock()
	if e.exited {
		// Closed or failed while detached: end the held command.
		e.mu.Unlock()
		_ = w.Write(FrameAbort, nil)
		_ = rw.Close()
		return nil
	}
	var resend []byte
	if resumed != nil {
		data, ok := e.in.From(resumed.Stdin)
		if !ok {
			e.mu.Unlock()
			err := fmt.Errorf("re-attach: the agent took %d bytes of input, not within %d-%d", resumed.Stdin, e.in.Base(), e.in.End())
			e.finish(ExitStatus{Code: -1}, err)
			_ = rw.Close()
			return err
		}
		resend = append([]byte(nil), data...)
		e.in.Trim(resumed.Stdin)
		e.cond.Broadcast()
	}
	e.gen++
	gen := e.gen
	e.conn, e.w = rw, w
	e.lastRecv = time.Now()
	eof, closed := e.inEOF, e.closed
	size := e.size
	if e.detached != nil {
		select {
		case <-e.detached:
			e.detached = make(chan struct{})
		default:
		}
	}
	e.mu.Unlock()

	if resumed != nil {
		if len(resend) > 0 {
			_ = w.WriteData(FrameData, resend)
		}
		if eof {
			_ = w.Write(FrameEOF, nil)
		}
		if size != nil && (size.Rows > 0 || size.Cols > 0) {
			_ = w.WriteJSON(FrameResize, *size)
		}
	}
	if closed {
		_ = w.Write(FrameAbort, nil)
		closeWrite(rw)
	}
	go e.read(gen, rw, r, w)
	if e.opts.Heartbeat {
		go e.heartbeat(gen, w)
	}
	return nil
}

// read reads the agent's frames on one attachment until it ends.
func (e *Exec) read(gen int, rw io.ReadWriteCloser, r *FrameReader, w *FrameWriter) {
	for {
		f, err := r.Next()
		if err != nil {
			e.lost(gen, rw, err)
			return
		}
		e.mu.Lock()
		e.lastRecv = time.Now()
		e.mu.Unlock()
		switch f.Type {
		case FrameData:
			_, _ = e.stdout.Write(f.Payload)
			e.received(w, int64(len(f.Payload)), 0)
		case FrameStderr:
			_, _ = e.stderr.Write(f.Payload)
			e.received(w, 0, int64(len(f.Payload)))
		case FramePing:
			var p Ping
			if DecodeFrame(f, &p) == nil && p.Stdin > 0 {
				e.mu.Lock()
				e.inAcked = max(e.inAcked, p.Stdin)
				e.in.Trim(p.Stdin)
				e.mu.Unlock()
				e.cond.Broadcast()
			}
		case FrameExit:
			var st ExitStatus
			if err := DecodeFrame(f, &st); err != nil {
				st = ExitStatus{Code: -1, Error: err.Error()}
			}
			if e.resumable {
				// Tell the agent the status arrived, so it stops holding.
				_ = w.Write(FrameExit, nil)
			}
			var serr error
			if st.Error != "" {
				serr = errors.New(st.Error)
			}
			// Release the stream before Wait can return.
			_ = rw.Close()
			e.finish(st, serr)
			return
		}
	}
}

// received counts output and acknowledges it every AckEvery bytes.
func (e *Exec) received(w *FrameWriter, out, errOut int64) {
	e.mu.Lock()
	e.outOff += out
	e.errOff += errOut
	var ack *Ping
	if e.resumable && (e.outOff-e.ackOut >= AckEvery || e.errOff-e.ackErr >= AckEvery) {
		e.ackOut, e.ackErr = e.outOff, e.errOff
		ack = &Ping{Stdout: e.outOff, Stderr: e.errOff}
	}
	e.mu.Unlock()
	if ack != nil {
		_ = w.WriteJSON(FramePing, *ack)
	}
}

// heartbeat pings the agent on a self-standing transport, acknowledging what
// arrived, and drops a transport that went silent.
func (e *Exec) heartbeat(gen int, w *FrameWriter) {
	t := time.NewTicker(StreamPingInterval)
	defer t.Stop()
	for {
		select {
		case <-e.done:
			return
		case <-t.C:
		}
		e.mu.Lock()
		if e.gen != gen {
			e.mu.Unlock()
			return
		}
		silent := time.Since(e.lastRecv) > StreamSilence
		conn := e.conn
		p := Ping{}
		if e.resumable {
			e.ackOut, e.ackErr = e.outOff, e.errOff
			p = Ping{Stdout: e.outOff, Stderr: e.errOff}
		}
		e.mu.Unlock()
		if silent {
			e.lost(gen, conn, errors.New("no frame for "+StreamSilence.String()))
			return
		}
		_ = w.WriteJSON(FramePing, p)
	}
}

// lost handles the end of an attachment without EXIT: a resumable command
// that is not being aborted waits to be re-attached; anything else has lost
// its status.
func (e *Exec) lost(gen int, rw io.ReadWriteCloser, cause error) {
	if rw != nil {
		_ = rw.Close()
	}
	e.mu.Lock()
	if gen != e.gen || e.exited {
		e.mu.Unlock()
		return
	}
	e.conn, e.w = nil, nil
	if e.resumable && !e.closed {
		close(e.detached)
		e.mu.Unlock()
		e.cond.Broadcast()
		return
	}
	e.mu.Unlock()
	e.finish(ExitStatus{Code: -1}, fmt.Errorf("%w: %v", ErrConnectionLost, cause))
}

// finish records how the command ended (the first end wins) and ends its
// output.
func (e *Exec) finish(st ExitStatus, err error) {
	e.mu.Lock()
	if e.exited {
		e.mu.Unlock()
		return
	}
	e.exited = true
	e.status, e.err = st, err
	e.conn, e.w = nil, nil
	e.mu.Unlock()
	e.cond.Broadcast()
	outErr := err
	if outErr == nil {
		outErr = io.EOF
	}
	e.stdout.closeWrite(outErr)
	e.stderr.closeWrite(outErr)
	close(e.done)
}

// Fail gives up on a detached command: it ends with err, as if its transport
// had been lost for good. It does nothing once the command has ended.
func (e *Exec) Fail(err error) {
	if err == nil {
		err = ErrConnectionLost
	}
	e.finish(ExitStatus{Code: -1}, err)
}

// ResumeID is the id that re-attaches the command; "" when it is not
// resumable.
func (e *Exec) ResumeID() string { return e.id }

// PID is the command's pid in the pod.
func (e *Exec) PID() int { return e.pid }

// Resumable reports whether the command survives a lost transport.
func (e *Exec) Resumable() bool { return e.resumable }

// Detached is closed while a resumable command has lost its transport and
// waits to be re-attached (Client.Reattach, ReattachOver) or failed (Fail).
func (e *Exec) Detached() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		return nil
	}
	return e.detached
}

// Done is closed when the command has ended.
func (e *Exec) Done() <-chan struct{} { return e.done }

// Status is how the command ended; valid once Done is closed.
func (e *Exec) Status() (ExitStatus, error) {
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, e.err
}

// Stdin feeds the command. Close half-closes it (no-op with a TTY).
func (e *Exec) Stdin() io.WriteCloser { return &e.stdin }

// Stdout is the command's output (with a TTY, stdout and stderr merged).
func (e *Exec) Stdout() io.Reader { return e.stdout }

// Stderr is the command's error output; empty with a TTY.
func (e *Exec) Stderr() io.Reader { return e.stderr }

// Resize sets the TTY's size; without a TTY it does nothing. A command
// waiting to be re-attached gets the latest size then.
func (e *Exec) Resize(rows, cols uint16) error {
	if !e.header.TTY {
		return nil
	}
	e.mu.Lock()
	e.size = &Resize{Rows: rows, Cols: cols}
	w := e.w
	e.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.WriteJSON(FrameResize, Resize{Rows: rows, Cols: cols})
}

// Signal sends sig (a name in Signals, without "SIG") to the command's
// foreground process group.
func (e *Exec) Signal(sig string) error {
	e.mu.Lock()
	w := e.w
	e.mu.Unlock()
	if w == nil {
		return ErrConnectionLost
	}
	return w.WriteJSON(FrameSignal, Signal{Signal: sig})
}

// Wait waits for the command to end and returns its exit code: -1 when a
// signal killed it, or with err when its status was lost.
func (e *Exec) Wait() (int, error) {
	st, err := e.Status()
	if err != nil {
		return -1, err
	}
	if st.Signal != "" {
		return -1, nil
	}
	return st.Code, nil
}

// Close aborts the command (the agent ends its whole process group) and ends
// Stdout and Stderr at once; Wait then returns once the agent reports the
// end. It does nothing once the command has ended.
func (e *Exec) Close() error {
	e.mu.Lock()
	if e.exited || e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	w, conn, gen := e.w, e.conn, e.gen
	e.mu.Unlock()
	e.cond.Broadcast()
	e.stdout.closeRead()
	e.stderr.closeRead()
	if w == nil {
		// Detached: a re-attach ends the held command (it sends ABORT), else
		// its grace in the pod does.
		e.finish(ExitStatus{Code: -1, Signal: "KILL"}, nil)
		return nil
	}
	_ = w.Write(FrameAbort, nil)
	closeWrite(conn)
	time.AfterFunc(abortWait, func() { e.lost(gen, conn, errors.New("no exit status after abort")) })
	return nil
}

// closeWrite half-closes rw when it can, else closes it.
func closeWrite(rw io.ReadWriteCloser) {
	if cw, ok := rw.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = rw.Close()
}

// execStdin is an Exec's stdin.
type execStdin struct{ e *Exec }

// Write sends p, keeping at most HoldBuffer the command has not consumed in
// flight (the agent's input window): Write waits while that is full, also
// while a resumable command waits to be re-attached, which resends what the
// agent did not get.
func (s *execStdin) Write(p []byte) (int, error) {
	e := s.e
	written := 0
	for len(p) > 0 {
		e.mu.Lock()
		for e.inEnd-e.inAcked >= HoldBuffer && !e.exited && !e.closed {
			e.cond.Wait()
		}
		e.mu.Unlock()

		e.sendMu.Lock()
		e.mu.Lock()
		if e.exited || e.closed || e.inEOF {
			e.mu.Unlock()
			e.sendMu.Unlock()
			return written, ErrStdinClosed
		}
		n := min(len(p), HoldBuffer-int(e.inEnd-e.inAcked))
		if e.resumable {
			e.in.Append(p[:n])
		}
		e.inEnd += int64(n)
		w := e.w
		e.mu.Unlock()
		var err error
		if w != nil {
			err = w.WriteData(FrameData, p[:n])
		}
		e.sendMu.Unlock()
		if err != nil && !e.resumable {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close half-closes the command's stdin; with a TTY, or once the command has
// ended, it does nothing.
func (s *execStdin) Close() error {
	e := s.e
	if e.header.TTY {
		return nil
	}
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	e.mu.Lock()
	if e.exited || e.closed || e.inEOF {
		e.mu.Unlock()
		return nil
	}
	e.inEOF = true
	w := e.w
	e.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.Write(FrameEOF, nil)
}

// firstFrame reads rw's first frame within ctx, closing rw if ctx ends first.
func firstFrame(ctx context.Context, rw io.Closer, r *FrameReader) (Frame, error) {
	type result struct {
		f   Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := r.Next()
		ch <- result{f, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return Frame{}, noEOF(res.err)
		}
		return res.f, nil
	case <-ctx.Done():
		_ = rw.Close()
		<-ch
		return Frame{}, ctx.Err()
	}
}

// RunResult is a buffered command's outcome.
type RunResult struct {
	Stdout, Stderr []byte
	// Code is the exit code; -1 when a signal (Signal) killed the command.
	Code   int
	Signal string
}

// Run runs a command to completion, feeding it stdin (nil: none) and
// buffering its output, at most MaxReply of stdout and stderr together: a
// command that prints more is aborted with ErrReplyTooLarge. ctx ending aborts
// it. This is how scripts and polls run; streams use Exec.
func (c *Client) Run(ctx context.Context, header ExecHeader, stdin io.Reader) (*RunResult, error) {
	e, err := c.Exec(ctx, header)
	if err != nil {
		return nil, err
	}
	return collect(ctx, e, stdin)
}

// collect drives e to its end for Run.
func collect(ctx context.Context, e *Exec, stdin io.Reader) (*RunResult, error) {
	stop := context.AfterFunc(ctx, func() { _ = e.Close() })
	defer stop()
	go func() {
		if stdin != nil {
			_, _ = io.Copy(e.Stdin(), stdin)
		}
		_ = e.Stdin().Close()
	}()
	var mu sync.Mutex
	total := 0
	tooLarge := false
	var wg sync.WaitGroup
	read := func(r io.Reader, dst *[]byte) {
		defer wg.Done()
		buf := make([]byte, DataChunk)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				mu.Lock()
				if total+n > MaxReply {
					tooLarge = true
					mu.Unlock()
					_ = e.Close()
					return
				}
				total += n
				*dst = append(*dst, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	res := &RunResult{}
	wg.Add(2)
	go read(e.Stdout(), &res.Stdout)
	go read(e.Stderr(), &res.Stderr)
	wg.Wait()
	st, err := e.Status()
	if tooLarge {
		return nil, ErrReplyTooLarge
	}
	if ctxErr := ctx.Err(); ctxErr != nil && e.closedByUs() {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	res.Code, res.Signal = st.Code, st.Signal
	return res, nil
}

func (e *Exec) closedByUs() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}
