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
	if cur != nil {
		cur.send(outFrame{t: FrameResult, payload: data})
	}
	timer := time.NewTimer(hold)
	defer timer.Stop()
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

	s.mu.Lock()
	old := s.cur
	s.cur = conn
	reply := Hello{Proto: ProtoVersion, Op: s.opts.Op, Gen: s.opts.Gen,
		Workspace: s.opts.Workspace && !s.wsDone, Done: s.result != nil}
	var result []byte
	if s.result != nil {
		result, _ = json.Marshal(*s.result)
	}
	s.mu.Unlock()
	if old != nil {
		s.logf("jobboot: a newer connection replaces the current one")
		old.close()
	}
	replyData, _ := json.Marshal(reply)
	conn.send(outFrame{t: FrameHello, payload: replyData})
	if result != nil {
		conn.send(outFrame{t: FrameResult, payload: result})
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
			s.mu.Lock()
			wanted := s.opts.Workspace && !s.wsDone
			s.mu.Unlock()
			if !wanted {
				rejecting = true
				conn.sendJSON(FrameError, ErrorMessage{Error: "the job takes no workspace now", Workspace: true})
				continue
			}
			rejecting = false
			conn.stream = s.newAttempt(conn)
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
// replacing an older transfer the job has not taken.
func (s *Server) newAttempt(conn *serverConn) *WorkspaceStream {
	ws := &WorkspaceStream{
		srv:   s,
		conn:  conn,
		items: make(chan wsItem, 8),
		abort: make(chan struct{}),
		hash:  sha256.New(),
		stall: DefaultStallTimeout,
	}
	select {
	case stale := <-s.attempts:
		// The job never took it, so nobody reads it: abandon it, which
		// also frees its connection's read loop.
		_ = stale.Close()
	default:
	}
	s.attempts <- ws
	return ws
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
			ws.err = ErrStalled
			return 0, ws.err
		}
		timer := time.NewTimer(left)
		select {
		case item := <-ws.items:
			timer.Stop()
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
		case <-ws.srv.closed:
			timer.Stop()
			ws.err = net.ErrClosed
			return 0, ws.err
		case <-timer.C:
		}
	}
}

// Accept tells fleetd the transfer was extracted; the job takes no other.
func (ws *WorkspaceStream) Accept() {
	ws.Close()
	ws.srv.mu.Lock()
	ws.srv.wsDone = true
	ws.srv.mu.Unlock()
	data, _ := json.Marshal(Progress{Phase: "workspace", Detail: "received", Done: true})
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
