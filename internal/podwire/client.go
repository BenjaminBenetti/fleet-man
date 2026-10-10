package podwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// ClientOptions configure Dial.
type ClientOptions struct {
	// Role is the session's role (RoleMain when empty).
	Role Role
	// Version is fleetd's build. An agent of another build is accepted with
	// HelloAck.RestageHint, as long as its protocol range overlaps.
	Version string
	// ProtoMin and ProtoMax are fleetd's protocol range (ProtoMin and
	// ProtoMax when zero).
	ProtoMin, ProtoMax int
	// Check vets the agent's Hello before fleetd accepts it; an error
	// refuses the agent with that reason.
	Check func(Hello) error
	// Logger receives the agent's LOG records, through a LogLimiter. Give it
	// fleet.log tagged with the pod's fleet and instance; nil drops them.
	Logger *slog.Logger
	// LogLimit and LogWindow override the limiter's LogLimit and LogWindow.
	LogLimit  int
	LogWindow time.Duration
	// LogLimiter, when set, takes the records instead of a limiter of the
	// session's own (Logger, LogLimit and LogWindow are then unused): give a
	// pod's agent and bulk sessions one between them, so the pod's limit
	// holds across both. The session never closes a limiter it was given.
	LogLimiter *LogLimiter
	// Handlers serve the streams the agent opens (the socket relays). A kind
	// without a handler is refused.
	Handlers map[Tag]Handler
	// PingInterval and Silence override PingInterval and DaemonSilence.
	PingInterval, Silence time.Duration
}

// Errors a Client's session ends with.
var (
	// ErrAgentSilent: nothing came from the agent for the silence deadline.
	ErrAgentSilent = errors.New("pod agent went silent")
	// ErrClosed: the session was closed on this side.
	ErrClosed = errors.New("pod agent session closed")
)

// RefusedError is an agent Dial refused (no common protocol version, or
// Check's error).
type RefusedError struct {
	Hello  Hello
	Reason string
}

func (e *RefusedError) Error() string { return "pod agent refused: " + e.Reason }

// Client is fleetd's end of one agent session. Its methods are safe for
// concurrent use.
type Client struct {
	opts  ClientOptions
	sess  *yamux.Session
	ctlW  *FrameWriter
	hello Hello
	proto int
	logs  *LogLimiter
	// ownLogs: logs is the session's own, closed with it.
	ownLogs bool
	mux     *Mux

	done     chan struct{}
	failOnce sync.Once
	err      error

	lastRecv  atomic.Int64
	active    atomic.Int64
	idleSince atomic.Int64

	slotsMu sync.Mutex
	slots   map[Tag]chan struct{}
}

// Dial runs the handshake over conn (a `pod-agent serve` exec's stdio) and
// returns the session once the agent is accepted. ctx bounds the handshake.
func Dial(ctx context.Context, conn io.ReadWriteCloser, opts ClientOptions) (*Client, error) {
	if opts.Role == "" {
		opts.Role = RoleMain
	}
	if opts.ProtoMin == 0 && opts.ProtoMax == 0 {
		opts.ProtoMin, opts.ProtoMax = ProtoMin, ProtoMax
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = PingInterval
	}
	if opts.Silence <= 0 {
		opts.Silence = DaemonSilence
	}
	sess, err := yamux.Client(conn, SessionConfig(opts.Role))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()

	c := &Client{opts: opts, sess: sess, done: make(chan struct{})}
	hello, ctl, r, err := c.handshake()
	if err != nil {
		_ = sess.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("pod agent handshake: %w", ctxErr)
		}
		return nil, err
	}
	if !stop() {
		_ = sess.Close()
		return nil, fmt.Errorf("pod agent handshake: %w", ctx.Err())
	}
	c.hello = hello
	c.logs = opts.LogLimiter
	if c.logs == nil {
		c.logs, c.ownLogs = NewLogLimiter(opts.Logger, opts.LogLimit, opts.LogWindow), true
	}
	c.touch()
	c.idleSince.Store(time.Now().UnixNano())
	c.mux = &Mux{Handlers: opts.Handlers, Refused: c.streamRefused}
	go c.readControl(ctl, r)
	go c.heartbeat()
	go func() { c.fail(c.mux.Serve(sess)) }()
	go func() {
		<-sess.CloseChan()
		c.fail(io.EOF)
	}()
	return c, nil
}

// handshake opens the control stream, reads Hello, and accepts or refuses.
func (c *Client) handshake() (Hello, *yamux.Stream, *FrameReader, error) {
	ctl, err := c.sess.OpenStream()
	if err != nil {
		return Hello{}, nil, nil, fmt.Errorf("open control stream: %w", err)
	}
	if err := WritePreamble(ctl, TagControl, ControlHeader{
		ProtoMin: c.opts.ProtoMin, ProtoMax: c.opts.ProtoMax, Role: c.opts.Role, Version: c.opts.Version,
	}); err != nil {
		return Hello{}, nil, nil, fmt.Errorf("write control preamble: %w", err)
	}
	r := NewFrameReader(ctl)
	c.ctlW = NewFrameWriter(ctl)
	var hello Hello
	for {
		f, err := r.Next()
		if err != nil {
			return Hello{}, nil, nil, fmt.Errorf("read hello: %w", noEOF(err))
		}
		if f.Type != FrameData {
			continue
		}
		var m ControlMessage
		if err := DecodeFrame(f, &m); err != nil {
			return Hello{}, nil, nil, err
		}
		if m.Type == MsgHello && m.Hello != nil {
			hello = *m.Hello
			break
		}
	}
	ack := HelloAck{Accept: true, RestageHint: hello.Version != c.opts.Version}
	proto, err := Negotiate(c.opts.ProtoMin, c.opts.ProtoMax, hello.ProtoMin, hello.ProtoMax)
	if err == nil && hello.Role != c.opts.Role {
		err = fmt.Errorf("agent serves role %q, not %q", hello.Role, c.opts.Role)
	}
	if err == nil && c.opts.Check != nil {
		err = c.opts.Check(hello)
	}
	if err != nil {
		ack = HelloAck{Reason: err.Error()}
	} else {
		ack.Proto = proto
	}
	werr := c.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgHelloAck, HelloAck: &ack})
	if !ack.Accept {
		// Refused whether or not the ack went out: a refused agent ends the
		// session as soon as it reads it, which can fail the write that
		// delivered it.
		return Hello{}, nil, nil, &RefusedError{Hello: hello, Reason: ack.Reason}
	}
	if werr != nil {
		return Hello{}, nil, nil, fmt.Errorf("write hello ack: %w", werr)
	}
	c.proto = proto
	return hello, ctl, r, nil
}

// readControl reads the control stream: heartbeats and LOG records.
func (c *Client) readControl(ctl *yamux.Stream, r *FrameReader) {
	defer ctl.Close()
	for {
		f, err := r.Next()
		if err != nil {
			c.fail(fmt.Errorf("control stream: %w", noEOF(err)))
			return
		}
		c.touch()
		if f.Type != FrameData {
			continue
		}
		var m ControlMessage
		if DecodeFrame(f, &m) != nil {
			continue
		}
		if m.Type == MsgLog && m.Log != nil {
			c.logs.Log(*m.Log)
		}
	}
}

// heartbeat pings the agent and closes a session that went silent.
func (c *Client) heartbeat() {
	t := time.NewTicker(c.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastRecv.Load())) > c.opts.Silence {
				c.fail(ErrAgentSilent)
				return
			}
			_ = c.ctlW.Write(FramePing, nil)
		}
	}
}

func (c *Client) touch() { c.lastRecv.Store(time.Now().UnixNano()) }

func (c *Client) streamRefused(tag Tag, reason string) {
	c.logs.Log(LogRecord{Level: "warn", Msg: "refused a stream from the agent", Attrs: map[string]string{"kind": tag.String(), "reason": reason}})
}

// fail ends the session with err (the first reason wins).
func (c *Client) fail(err error) {
	c.failOnce.Do(func() {
		if err == nil {
			err = io.EOF
		}
		c.err = err
		_ = c.sess.Close()
		close(c.done)
		if c.ownLogs {
			c.logs.Close()
		}
	})
}

// Hello is what the agent said it is.
func (c *Client) Hello() Hello { return c.hello }

// Proto is the negotiated protocol version.
func (c *Client) Proto() int { return c.proto }

// RestageHint reports whether the agent is another build than fleetd's.
func (c *Client) RestageHint() bool { return c.hello.Version != c.opts.Version }

// Done is closed when the session ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the session ended; nil while it runs.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// closeSendWait bounds Close's attempt to tell the agent before it closes: a
// stalled transport must not hold Close up for yamux's write timeout.
const closeSendWait = 500 * time.Millisecond

// Close ends the session at once: it tells the agent this is a clean close
// (it holds its resumable commands and aborts the rest), then closes.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeSendWait)
	defer cancel()
	_ = c.sendClose(ctx)
	c.fail(ErrClosed)
	return nil
}

// Shutdown tells the agent to close and waits, within ctx, for it to end the
// session, then closes whatever is left.
func (c *Client) Shutdown(ctx context.Context) error {
	if err := c.sendClose(ctx); err == nil {
		select {
		case <-c.done:
		case <-ctx.Done():
		}
	}
	c.fail(ErrClosed)
	return nil
}

// sendClose sends Close on the control stream, giving up when ctx ends first.
func (c *Client) sendClose(ctx context.Context) error {
	sent := make(chan error, 1)
	go func() { sent <- c.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgClose}) }()
	select {
	case err := <-sent:
		return err
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Active is how many streams fleetd has open on the session.
func (c *Client) Active() int { return int(c.active.Load()) }

// IdleSince is when the session's last stream ended; zero while one is open.
// The bulk session is closed once it has been idle long enough.
func (c *Client) IdleSince() time.Time {
	if c.active.Load() > 0 {
		return time.Time{}
	}
	return time.Unix(0, c.idleSince.Load())
}

// Open opens a raw stream of kind tag, with header as its preamble, for a
// stream this package has no client for (a forward or relay is bytes after
// its preamble). The conn's CloseWrite sends the stream's FIN and leaves the
// agent's answer readable; Close sends it and discards whatever else arrives.
// Like every stream, it waits (within ctx) for one of its kind's StreamCap
// slots, and holds it until the agent's FIN.
func (c *Client) Open(ctx context.Context, tag Tag, header any) (net.Conn, error) {
	return c.open(ctx, tag, header)
}

// open opens a stream of kind tag with header, once fewer than the kind's
// StreamCap are open: the agent refuses a stream over its cap, so a burst
// waits here instead of failing there.
func (c *Client) open(ctx context.Context, tag Tag, header any) (*yamuxTransport, error) {
	if err := c.Err(); err != nil {
		return nil, fmt.Errorf("pod agent session: %w", err)
	}
	slot := c.slot(tag)
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("open %s stream: %w", tag, ctx.Err())
	case <-c.done:
		return nil, fmt.Errorf("pod agent session: %w", c.err)
	}
	release := func() {
		<-slot
		if c.active.Add(-1) == 0 {
			c.idleSince.Store(time.Now().UnixNano())
		}
	}
	c.active.Add(1)
	st, err := c.sess.OpenStream()
	if err != nil {
		release()
		return nil, fmt.Errorf("open %s stream: %w", tag, err)
	}
	if err := WritePreamble(st, tag, header); err != nil {
		_ = st.Close()
		release()
		return nil, fmt.Errorf("open %s stream: %w", tag, err)
	}
	return &yamuxTransport{Stream: st, end: release}, nil
}

// slot is the semaphore of tag's StreamCap.
func (c *Client) slot(tag Tag) chan struct{} {
	c.slotsMu.Lock()
	defer c.slotsMu.Unlock()
	if c.slots == nil {
		c.slots = map[Tag]chan struct{}{}
	}
	ch, ok := c.slots[tag]
	if !ok {
		ch = make(chan struct{}, max(StreamCap(tag), 1))
		c.slots[tag] = ch
	}
	return ch
}

// yamuxTransport is a yamux stream as a client transport. Close sends the
// stream's FIN and keeps the stream's slot until the agent's FIN arrives:
// the agent counts the stream against its cap until its handler is done.
type yamuxTransport struct {
	*yamux.Stream
	once sync.Once
	end  func()
}

// finWait bounds how long a closed stream's slot waits for the agent's FIN.
const finWait = 30 * time.Second

func (t *yamuxTransport) CloseWrite() error { return t.Stream.Close() }

func (t *yamuxTransport) Close() error {
	err := t.Stream.Close()
	t.once.Do(func() {
		go func() {
			defer t.end()
			_ = t.Stream.SetReadDeadline(time.Now().Add(finWait))
			_, _ = io.Copy(io.Discard, t.Stream)
		}()
	})
	return err
}
