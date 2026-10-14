package jobboot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Errors a job sees from the server.
var (
	// ErrStalled: StallTimeout passed without a byte from fleetd while the
	// job waited on it. The job exits ExitStalled.
	ErrStalled = errors.New("no bytes from fleetd within the stall timeout")
	// ErrDropped: the connection carrying a workspace transfer ended (or a
	// newer one replaced it) before the transfer did.
	ErrDropped = errors.New("the workspace transfer's connection dropped")
	// ErrChecksum: a workspace transfer's bytes do not match the length and
	// sha256 fleetd sent at its end.
	ErrChecksum = errors.New("workspace checksum mismatch")
	// ErrNotCollected: the job finished but fleetd did not acknowledge its
	// result within the hold.
	ErrNotCollected = errors.New("fleetd did not collect the job's result")
)

// ServerOptions configure Listen.
type ServerOptions struct {
	// Op and Gen name the job in its hello.
	Op  string
	Gen int
	// Workspace is whether the job takes a workspace tar.
	Workspace bool
	// Logf receives notes about connections (who connected, what dropped);
	// nil drops them.
	Logf func(format string, args ...any)
	// HelloTimeout bounds a new connection's hello; 0 is 10s.
	HelloTimeout time.Duration
}

// Server is the job's end of the job socket. It holds one fleetd connection
// at a time (a newer one replaces an older one, which may be half dead), hands
// the job each workspace transfer as a WorkspaceStream, sends its progress and
// keeps its result until fleetd acknowledges it.
type Server struct {
	ln   net.Listener
	opts ServerOptions

	mu       sync.Mutex
	cur      *serverConn
	wsDone   bool
	result   *Result
	lastByte time.Time
	// barrier is the one the job waits at, if any (Barrier); barrierSeq
	// numbers them.
	barrier    *pendingBarrier
	barrierSeq int
	// attempts holds the newest workspace transfer the job has not taken
	// yet (at most one: a newer transfer replaces it).
	attempts chan *WorkspaceStream

	acked   chan struct{}
	ackOnce sync.Once
	closed  chan struct{}
	wg      sync.WaitGroup
}

// Listen starts a Server on the unix socket at socket, replacing a stale
// socket file left by an earlier run.
func Listen(socket string, opts ServerOptions) (*Server, error) {
	if opts.HelloTimeout <= 0 {
		opts.HelloTimeout = 10 * time.Second
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale job socket: %w", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen on the job socket: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod the job socket: %w", err)
	}
	s := &Server{
		ln:       ln,
		opts:     opts,
		lastByte: time.Now(),
		attempts: make(chan *WorkspaceStream, 1),
		acked:    make(chan struct{}),
		closed:   make(chan struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Close stops the server and drops its connection.
func (s *Server) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)
	err := s.ln.Close()
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur != nil {
		cur.close()
	}
	s.wg.Wait()
	return err
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// touch records a byte from fleetd, for the stall watchdog.
func (s *Server) touch() {
	s.mu.Lock()
	s.lastByte = time.Now()
	s.mu.Unlock()
}

// idleFor is how long it has been since fleetd's last byte.
func (s *Server) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastByte)
}

// Workspace waits for the next workspace transfer and returns it. It fails
// with ErrStalled once stall (0 is DefaultStallTimeout) passes without a byte
// from fleetd, and with ctx's error when ctx ends.
func (s *Server) Workspace(ctx context.Context, stall time.Duration) (*WorkspaceStream, error) {
	if stall <= 0 {
		stall = DefaultStallTimeout
	}
	for {
		left := stall - s.idleFor()
		if left <= 0 {
			// A transfer that already arrived is no stall.
			select {
			case ws := <-s.attempts:
				ws.stall = stall
				return ws, nil
			default:
			}
			return nil, ErrStalled
		}
		timer := time.NewTimer(left)
		select {
		case ws := <-s.attempts:
			timer.Stop()
			ws.stall = stall
			return ws, nil
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-s.closed:
			timer.Stop()
			return nil, net.ErrClosed
		case <-timer.C:
		}
	}
}

// Progress sends p to fleetd when one is connected. It never blocks the job:
// a connection that is not reading loses progress, never the result.
func (s *Server) Progress(p Progress) {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		return
	}
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	cur.trySend(outFrame{t: FrameProgress, payload: data})
}

// pendingBarrier is a barrier the job waits at: its frame, sent again to a
// newer connection, and where its release goes.
type pendingBarrier struct {
	b    Barrier
	data []byte
	ch   chan Release
}

// Barrier stops the job at a point fleetd must act at (a clone's copier
// before its final delta), sending name to fleetd (now, or to whichever
// connection comes next) and waiting for its release. It fails with
// ErrStalled once stall (0 is DefaultStallTimeout) passes without a byte from
// fleetd since the wait began (a release that says fleetd is still working
// counts), and with ctx's error when ctx ends. A release that refuses is
// returned as it is, with a nil error: the job decides.
func (s *Server) Barrier(ctx context.Context, name, detail string, stall time.Duration) (Release, error) {
	if stall <= 0 {
		stall = DefaultStallTimeout
	}
	s.mu.Lock()
	if s.barrier != nil {
		s.mu.Unlock()
		return Release{}, fmt.Errorf("jobboot: barrier %q while %q waits", name, s.barrier.b.Name)
	}
	s.barrierSeq++
	b := Barrier{Seq: s.barrierSeq, Name: name, Detail: detail}
	data, err := json.Marshal(b)
	if err != nil {
		s.mu.Unlock()
		return Release{}, err
	}
	pb := &pendingBarrier{b: b, data: data, ch: make(chan Release, 1)}
	s.barrier = pb
	cur := s.cur
	// fleetd's silence is counted from here: it had nothing to say while the
	// job worked.
	s.lastByte = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.barrier == pb {
			s.barrier = nil
		}
		s.mu.Unlock()
	}()
	if cur != nil {
		cur.send(outFrame{t: FrameBarrier, payload: data})
	}
	for {
		left := stall - s.idleFor()
		if left <= 0 {
			select {
			case r := <-pb.ch:
				return r, nil
			default:
			}
			return Release{}, ErrStalled
		}
		timer := time.NewTimer(left)
		select {
		case r := <-pb.ch:
			timer.Stop()
			return r, nil
		case <-ctx.Done():
			timer.Stop()
			return Release{}, ctx.Err()
		case <-s.closed:
			timer.Stop()
			return Release{}, net.ErrClosed
		case <-timer.C:
		}
	}
}

// release hands a release frame to the barrier it answers; one for no
// waiting barrier (an older one's, late) is dropped.
func (s *Server) release(r Release) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pb := s.barrier
	if r.Wait || pb == nil || pb.b.Seq != r.Seq {
		return
	}
	s.barrier = nil
	pb.ch <- r
}

// Finish records the job's result, sends it to fleetd (now, or to whichever
// connection comes next) and waits up to hold (0 is DefaultResultHold) for
// fleetd to acknowledge it. It returns ErrNotCollected when the hold passes
// first, and ctx's error when ctx ends.
func (s *Server) Finish(ctx context.Context, r Result, hold time.Duration) error {
	if hold <= 0 {
		hold = DefaultResultHold
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.result = &r
	cur := s.cur
	s.mu.Unlock()
	timer := time.NewTimer(hold)
	defer timer.Stop()
	if cur != nil {
		// The hold bounds this too: a connection whose reader stopped keeps
		// its queue full until it ends, and a newer connection gets the
		// result from serve.
		select {
		case cur.out <- outFrame{t: FrameResult, payload: data}:
		case <-cur.done:
		case <-timer.C:
			return ErrNotCollected
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closed:
			return net.ErrClosed
		}
	}
	select {
	case <-s.acked:
		return nil
	case <-timer.C:
		return ErrNotCollected
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return net.ErrClosed
	}
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.logf("jobboot: accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go s.serve(c)
	}
}

// serve runs one fleetd connection: the hello, then its frames until it ends.
func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	_ = c.SetReadDeadline(time.Now().Add(s.opts.HelloTimeout))
	t, payload, err := ReadFrame(c)
	if err != nil || t != FrameHello {
		s.logf("jobboot: connection without a hello (%v, %s)", err, t)
		_ = c.Close()
		return
	}
	var hello Hello
	if err := json.Unmarshal(payload, &hello); err != nil || hello.Proto != ProtoVersion {
		_ = WriteJSON(c, FrameError, ErrorMessage{Error: fmt.Sprintf("this job speaks jobboot %d, not %d", ProtoVersion, hello.Proto)})
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	s.touch()

	conn := newServerConn(c)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		conn.writeLoop()
	}()

	// The hello is queued before the connection becomes current, so a
	// result Finish sends to it can never overtake it. A fresh queue always
	// has room for both frames.
	s.mu.Lock()
	old := s.cur
	replyData, _ := json.Marshal(Hello{Proto: ProtoVersion, Op: s.opts.Op, Gen: s.opts.Gen,
		Workspace: s.opts.Workspace && !s.wsDone, Done: s.result != nil})
	conn.out <- outFrame{t: FrameHello, payload: replyData}
	if s.result != nil {
		result, _ := json.Marshal(*s.result)
		conn.out <- outFrame{t: FrameResult, payload: result}
	} else if s.barrier != nil {
		conn.out <- outFrame{t: FrameBarrier, payload: s.barrier.data}
	}
	s.cur = conn
	s.mu.Unlock()
	if old != nil {
		s.logf("jobboot: a newer connection replaces the current one")
		old.close()
	}

	err = s.readLoop(conn)
	if conn.stream != nil {
		conn.stream.finish(ErrDropped)
		conn.stream = nil
	}
	conn.close()
	s.mu.Lock()
	if s.cur == conn {
		s.cur = nil
	}
	s.mu.Unlock()
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.logf("jobboot: connection ended: %v", err)
	}
}

// readLoop handles conn's frames until it ends or breaks the protocol.
func (s *Server) readLoop(conn *serverConn) error {
	// rejecting is set after a refused FrameWorkspace: that transfer's data
	// is skipped, not treated as a protocol error.
	rejecting := false
	for {
		t, payload, err := ReadFrame(conn.c)
		if err != nil {
			return err
		}
		if t != FramePing {
			s.touch()
		}
		switch t {
		case FramePing:
		case FrameWorkspace:
			if conn.stream != nil {
				return errors.New("a workspace transfer started inside another")
			}
			ws := s.newAttempt(conn)
			if ws == nil {
				rejecting = true
				conn.sendJSON(FrameError, ErrorMessage{Error: "the job takes no workspace now", Workspace: true})
				continue
			}
			rejecting = false
			conn.stream = ws
		case FrameData:
			if rejecting {
				continue
			}
			if conn.stream == nil {
				return errors.New("workspace data outside a transfer")
			}
			if !conn.stream.produce(payload) {
				// The job abandoned this transfer; skip the rest of it.
				conn.stream = nil
				rejecting = true
			}
		case FrameEnd:
			if rejecting {
				rejecting = false
				continue
			}
			if conn.stream == nil {
				return errors.New("workspace end outside a transfer")
			}
			var end WorkspaceEnd
			if err := json.Unmarshal(payload, &end); err != nil {
				return fmt.Errorf("bad workspace end: %w", err)
			}
			conn.stream.end(end)
			conn.stream = nil
		case FrameAck:
			s.mu.Lock()
			done := s.result != nil
			s.mu.Unlock()
			if done {
				s.ackOnce.Do(func() { close(s.acked) })
			}
		case FrameRelease:
			var r Release
			if err := json.Unmarshal(payload, &r); err != nil {
				return fmt.Errorf("bad release: %w", err)
			}
			s.release(r)
		case FrameError:
			var msg ErrorMessage
			_ = json.Unmarshal(payload, &msg)
			s.logf("jobboot: fleetd reports: %s", msg.Error)
		default:
			return fmt.Errorf("unexpected %s frame from fleetd", t)
		}
	}
}

// newAttempt starts a workspace transfer on conn and offers it to the job,
// replacing an older transfer the job has not taken; nil when the job takes
// no workspace now. Every offer is made under mu, right after emptying the
// slot, so it never blocks; Accept closes the slot under mu too.
func (s *Server) newAttempt(conn *serverConn) *WorkspaceStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.opts.Workspace || s.wsDone {
		return nil
	}
	ws := &WorkspaceStream{
		srv:   s,
		conn:  conn,
		items: make(chan wsItem, 8),
		abort: make(chan struct{}),
		hash:  sha256.New(),
		stall: DefaultStallTimeout,
	}
	s.dropStaleAttemptLocked()
	s.attempts <- ws
	return ws
}

// dropStaleAttemptLocked abandons the transfer waiting in the slot, if any:
// the job never took it, so nobody reads it, and closing it frees its
// connection's read loop to skip the rest of its data. s.mu is held.
func (s *Server) dropStaleAttemptLocked() {
	select {
	case stale := <-s.attempts:
		_ = stale.Close()
	default:
	}
}

// wsItem is the next piece of a transfer: data, or its end (err nil when it
// verified).
type wsItem struct {
	data  []byte
	final bool
	err   error
}

// WorkspaceStream is one workspace transfer, as the job reads it. Read yields
// the tar's bytes and then io.EOF only once the whole stream matched the
// length and sha256 fleetd sent at its end; a mismatch, a dropped connection or
// a stall is an error instead, after which the job discards what it extracted
// (Reject) and waits for the next transfer.
type WorkspaceStream struct {
	srv  *Server
	conn *serverConn

	items chan wsItem
	abort chan struct{}
	// Producer state (the connection's read loop).
	hash      hash.Hash
	n         int64
	finishOne sync.Once

	// Consumer state (the job).
	stall time.Duration
	buf   []byte
	err   error
	close sync.Once
}

// produce hands the job one chunk; false once the job has abandoned the
// transfer.
func (ws *WorkspaceStream) produce(data []byte) bool {
	ws.hash.Write(data)
	ws.n += int64(len(data))
	select {
	case ws.items <- wsItem{data: data}:
		return true
	case <-ws.abort:
		return false
	case <-ws.srv.closed:
		return false
	}
}

// end checks the transfer against fleetd's trailer and ends it.
func (ws *WorkspaceStream) end(e WorkspaceEnd) {
	got := hex.EncodeToString(ws.hash.Sum(nil))
	if e.Bytes != ws.n || e.SHA256 != got {
		ws.finish(fmt.Errorf("%w: received %d bytes with sha256 %s, fleetd sent %d bytes with sha256 %s",
			ErrChecksum, ws.n, got, e.Bytes, e.SHA256))
		return
	}
	ws.finish(nil)
}

// finish ends the transfer with err (nil: verified).
func (ws *WorkspaceStream) finish(err error) {
	ws.finishOne.Do(func() {
		select {
		case ws.items <- wsItem{final: true, err: err}:
		case <-ws.abort:
		case <-ws.srv.closed:
		}
	})
}

// Read reads the transfer's bytes.
func (ws *WorkspaceStream) Read(p []byte) (int, error) {
	if len(ws.buf) > 0 {
		n := copy(p, ws.buf)
		ws.buf = ws.buf[n:]
		return n, nil
	}
	if ws.err != nil {
		return 0, ws.err
	}
	for {
		left := ws.stall - ws.srv.idleFor()
		if left <= 0 {
			// What already arrived is read first: a stall is fleetd's
			// silence with nothing left to read, not a slow extraction.
			select {
			case item := <-ws.items:
				return ws.take(item, p)
			default:
			}
			ws.err = ErrStalled
			return 0, ws.err
		}
		timer := time.NewTimer(left)
		select {
		case item := <-ws.items:
			timer.Stop()
			return ws.take(item, p)
		case <-ws.srv.closed:
			timer.Stop()
			ws.err = net.ErrClosed
			return 0, ws.err
		case <-timer.C:
		}
	}
}

// take hands Read one item of the transfer.
func (ws *WorkspaceStream) take(item wsItem, p []byte) (int, error) {
	if item.final {
		ws.err = item.err
		if ws.err == nil {
			ws.err = io.EOF
		}
		return 0, ws.err
	}
	n := copy(p, item.data)
	ws.buf = item.data[n:]
	return n, nil
}

// Accept tells fleetd the transfer was extracted (files entries, bytes of
// content); the job takes no other. A transfer a newer connection started
// meanwhile is abandoned, so that connection's read loop skips its data
// instead of blocking on it (and missing the acknowledgement of the result).
func (ws *WorkspaceStream) Accept(files, bytes int64) {
	ws.Close()
	ws.srv.mu.Lock()
	ws.srv.wsDone = true
	ws.srv.dropStaleAttemptLocked()
	ws.srv.mu.Unlock()
	data, _ := json.Marshal(Progress{Phase: "workspace", Detail: "received", Files: files, Bytes: bytes, Done: true})
	ws.conn.send(outFrame{t: FrameProgress, payload: data})
}

// Reject tells fleetd the transfer failed with err; the job waits for
// another.
func (ws *WorkspaceStream) Reject(err error) {
	ws.Close()
	ws.conn.sendJSON(FrameError, ErrorMessage{Error: err.Error(), Workspace: true})
}

// Close abandons the transfer: its remaining data is skipped.
func (ws *WorkspaceStream) Close() error {
	ws.close.Do(func() { close(ws.abort) })
	return nil
}

// outFrame is a frame queued for a connection's writer.
type outFrame struct {
	t       FrameType
	payload []byte
}

// serverConn is one fleetd connection. A single writer goroutine owns its
// writes; the job's Progress drops frames rather than wait on a slow reader.
type serverConn struct {
	c      net.Conn
	out    chan outFrame
	done   chan struct{}
	closed sync.Once
	// stream is the transfer in flight on this connection (read loop only).
	stream *WorkspaceStream
}

func newServerConn(c net.Conn) *serverConn {
	return &serverConn{c: c, out: make(chan outFrame, 32), done: make(chan struct{})}
}

func (sc *serverConn) writeLoop() {
	for {
		select {
		case f := <-sc.out:
			if err := WriteFrame(sc.c, f.t, f.payload); err != nil {
				sc.close()
				return
			}
		case <-sc.done:
			return
		}
	}
}

// send queues f, waiting for room unless the connection ends.
func (sc *serverConn) send(f outFrame) {
	select {
	case sc.out <- f:
	case <-sc.done:
	}
}

func (sc *serverConn) sendJSON(t FrameType, v any) {
	data, err := json.Marshal(v)
	if err == nil {
		sc.send(outFrame{t: t, payload: data})
	}
}

// trySend queues f only if there is room.
func (sc *serverConn) trySend(f outFrame) {
	select {
	case sc.out <- f:
	default:
	}
}

func (sc *serverConn) close() {
	sc.closed.Do(func() {
		close(sc.done)
		_ = sc.c.Close()
	})
}
