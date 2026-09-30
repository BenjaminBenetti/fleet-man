// Package mcpbridge connects a stdio MCP client inside an instance — a coding
// agent that was given the fleet MCP server (issue #219) — to the fleet
// daemon, which serves MCP on a unix socket in the instance's control
// directory (ContainerSocketPath). Both sides speak MCP's stdio framing,
// newline-delimited JSON-RPC, and each connection is one daemon-side MCP
// session, so while the daemon stays up the bridge is a plain pipe.
//
// It does a little more so the agent keeps its fleet tools across a daemon
// restart (a fleet upgrade, "Restart daemon"): when the socket drops, the
// requests still in flight are answered with an error, and the next message
// from the client reconnects and first replays the client's initialize
// handshake — swallowing the reply — so the client carries on with a fresh
// daemon-side session it never sees.
package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

const (
	// SocketName is the daemon's MCP socket's basename in an instance's
	// control directory (next to control.SocketName).
	SocketName = "mcp.sock"
	// ContainerSocketPath is where an instance sees the daemon's MCP socket.
	ContainerSocketPath = control.ContainerMountDir + "/" + SocketName
	// Subcommand is the (hidden) fleet subcommand that runs the bridge.
	Subcommand = "mcp-bridge"
)

// timings bounds the bridge's waits.
type timings struct {
	// connect bounds how long a message waits for the daemon's socket (it
	// may be restarting) before a request is answered with an error.
	connect time.Duration
	// replay bounds the daemon's answer to a replayed initialize.
	replay time.Duration
	// drain bounds how long the bridge keeps relaying replies once the
	// client has closed its side, for requests it sent just before.
	drain time.Duration
	// dialRetry spaces dial attempts while waiting for the socket.
	dialRetry time.Duration
	// failFast: after a connect gives up, messages are answered at once for
	// this long instead of each waiting out another connect, so requests
	// queued during a long outage are not failed one connect apart.
	failFast time.Duration
}

var defaultTimings = timings{
	connect:   10 * time.Second,
	replay:    10 * time.Second,
	drain:     30 * time.Second,
	dialRetry: 250 * time.Millisecond,
	failFast:  2 * time.Second,
}

// errCodeUnavailable is the JSON-RPC error code of a request the bridge
// answers itself because the daemon could not (JSON-RPC "internal error").
const errCodeUnavailable = -32603

// Why a request the daemon never answered failed.
const (
	// errLost: the connection ended after the daemon had answered on it —
	// normally the daemon restarting.
	errLost = "lost the connection to the fleet daemon (restarting?); retry the call"
	// errRefused: the daemon closed a connection without ever answering on
	// it — normally this process is not one it serves (the socket's peer
	// check), which its fleet.log says.
	errRefused = "the fleet daemon closed the connection without answering; this process may not be allowed to use the fleet MCP here (see the daemon's fleet.log)"
)

// Bridge relays MCP messages between a stdio client and the daemon.
type Bridge struct {
	// Dial connects to the daemon's MCP socket.
	Dial func(ctx context.Context) (net.Conn, error)
	// In carries the client's messages; Out receives the daemon's.
	In  io.Reader
	Out io.Writer

	// timing is defaultTimings unless a test shortens it.
	timing timings

	// connectMu serializes connecting, so one message at a time dials. It
	// also guards downUntil/downErr: the last connect's failure, reused
	// without dialing until downUntil (timing.failFast).
	connectMu sync.Mutex
	downUntil time.Time
	downErr   error

	mu   sync.Mutex
	conn net.Conn // nil while disconnected
	// pending holds the requests sent on conn that have no reply yet, by
	// normalized id, with the id as the client wrote it.
	pending map[string]json.RawMessage
	// idle is closed (and replaced) whenever pending empties.
	idle chan struct{}
	// initialize / initialized are the client's handshake, replayed on a
	// reconnect.
	initialize  []byte
	initialized []byte
	replays     int

	outMu sync.Mutex
}

// Dialer returns a Dial for the unix socket at path.
func Dialer(path string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

// message is the part of a JSON-RPC message the bridge looks at.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
}

func (m message) hasID() bool {
	return len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null"))
}

// Run relays until the client closes its input (then, after the replies it
// is still owed, returns) or ctx ends — a client stopping its server with a
// signal — and returns nil either way. An error means stdin failed.
func (b *Bridge) Run(ctx context.Context) error {
	if b.timing == (timings{}) {
		b.timing = defaultTimings
	}
	b.mu.Lock()
	b.pending = make(map[string]json.RawMessage)
	b.idle = make(chan struct{})
	close(b.idle)
	b.mu.Unlock()
	defer b.disconnect()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReader(b.In)
		for {
			line, err := r.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case line := <-lines:
			b.fromClient(ctx, line)
		case err := <-readErr:
			if !errors.Is(err, io.EOF) {
				return err
			}
			b.drain(ctx)
			return nil
		}
	}
}

// fromClient forwards one client message, connecting first if need be.
func (b *Bridge) fromClient(ctx context.Context, line []byte) {
	var msg message
	parsed := json.Unmarshal(line, &msg) == nil
	isRequest := parsed && msg.Method != "" && msg.hasID()
	if parsed && msg.Method == "initialize" {
		b.mu.Lock()
		b.initialize = bytes.Clone(line)
		b.initialized = nil
		b.mu.Unlock()
	}

	// A connection the daemon has just closed can still be the live one here
	// (its reader has not seen the end yet): a write to it fails without
	// delivering anything, so the message is sent once more on a fresh one.
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := b.connection(ctx, msg.Method == "initialize")
		if err != nil {
			if isRequest {
				b.replyError(msg.ID, fmt.Sprintf("the fleet daemon is unreachable (%v); retry shortly", err))
			}
			return
		}
		b.mu.Lock()
		if b.conn != conn {
			// Dropped between connecting and here: nothing went out.
			b.mu.Unlock()
			continue
		}
		if isRequest {
			// Registered before the write, so the reply cannot beat it.
			b.pending[idKey(msg.ID)] = msg.ID
			b.markBusy()
		}
		b.mu.Unlock()
		if _, err := conn.Write(line); err == nil {
			if parsed && msg.Method == "notifications/initialized" {
				// Recorded once sent, so a reconnect that happens while
				// sending it does not replay it as well.
				b.mu.Lock()
				b.initialized = bytes.Clone(line)
				b.mu.Unlock()
			}
			return
		}
		// Unless dropping the connection already answered the request (its
		// reader got there first), take it back and try again.
		retry := true
		if isRequest {
			b.mu.Lock()
			key := idKey(msg.ID)
			_, retry = b.pending[key]
			delete(b.pending, key)
			b.markIdleIfDone()
			b.mu.Unlock()
		}
		b.drop(conn, errLost)
		if !retry {
			return
		}
	}
	if isRequest {
		b.replyError(msg.ID, errLost)
	}
}

// connection returns the live connection, connecting when there is none. A
// reconnect replays the client's handshake first, unless the message about to
// be sent is itself the initialize.
func (b *Bridge) connection(ctx context.Context, sendingInitialize bool) (net.Conn, error) {
	b.connectMu.Lock()
	defer b.connectMu.Unlock()
	b.mu.Lock()
	if b.conn != nil {
		conn := b.conn
		b.mu.Unlock()
		return conn, nil
	}
	initialize, initialized := b.initialize, b.initialized
	b.mu.Unlock()

	if time.Now().Before(b.downUntil) {
		return nil, b.downErr
	}
	conn, err := b.dial(ctx)
	if err != nil {
		b.downUntil, b.downErr = time.Now().Add(b.timing.failFast), err
		return nil, err
	}
	var replayID string
	var replayed chan struct{}
	if initialize != nil && !sendingInitialize {
		b.mu.Lock()
		b.replays++
		replayID = strconv.Quote("fleet-bridge-replay-" + strconv.Itoa(b.replays))
		b.mu.Unlock()
		replayed = make(chan struct{})
	}
	done := make(chan struct{})
	go b.readLoop(conn, replayID, replayed, done)

	if replayed != nil {
		if err := b.replay(conn, initialize, initialized, replayID, replayed, done); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()
	return conn, nil
}

// dial connects, retrying while the socket is missing or refusing (a daemon
// restarting) for up to timing.connect.
func (b *Bridge) dial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timing.connect)
	defer cancel()
	for {
		conn, err := b.Dial(ctx)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(b.timing.dialRetry):
		}
	}
}

// replay sends the client's initialize (under the bridge's own id) on a fresh
// connection, waits for the daemon's reply, then sends the client's
// initialized notification, so the new session is ready for the client's
// traffic.
func (b *Bridge) replay(conn net.Conn, initialize, initialized []byte, replayID string, replayed, done chan struct{}) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(initialize, &fields); err != nil {
		return fmt.Errorf("replay initialize: %w", err)
	}
	fields["id"] = json.RawMessage(replayID)
	req, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("replay initialize: %w", err)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("replay initialize: %w", err)
	}
	timer := time.NewTimer(b.timing.replay)
	defer timer.Stop()
	select {
	case <-replayed:
	case <-done:
		return errors.New("the fleet daemon closed the connection during the handshake")
	case <-timer.C:
		return errors.New("the fleet daemon did not answer the handshake")
	}
	if initialized != nil {
		if _, err := conn.Write(initialized); err != nil {
			return fmt.Errorf("replay initialized: %w", err)
		}
	}
	return nil
}

// readLoop relays the daemon's messages on conn to the client until conn
// ends. The reply to a replayed initialize (replayID) is swallowed.
func (b *Bridge) readLoop(conn net.Conn, replayID string, replayed, done chan struct{}) {
	defer close(done)
	answered := false
	defer func() {
		why := errLost
		if !answered {
			why = errRefused
		}
		b.drop(conn, why)
	}()
	var replayKey string
	if replayID != "" {
		replayKey = idKey(json.RawMessage(replayID))
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			answered = true
			var msg message
			if json.Unmarshal(line, &msg) == nil && msg.Method == "" && msg.hasID() {
				key := idKey(msg.ID)
				if replayKey != "" && key == replayKey {
					close(replayed)
					replayKey = ""
					continue
				}
				b.mu.Lock()
				delete(b.pending, key)
				b.markIdleIfDone()
				b.mu.Unlock()
			}
			b.write(line)
		}
		if err != nil {
			return
		}
	}
}

// drop retires conn (if it is still the live connection) and answers the
// requests that were waiting on it with why.
func (b *Bridge) drop(conn net.Conn, why string) {
	_ = conn.Close()
	b.mu.Lock()
	if b.conn != conn {
		b.mu.Unlock()
		return
	}
	b.conn = nil
	lost := b.pending
	b.pending = make(map[string]json.RawMessage)
	b.markIdleIfDone()
	b.mu.Unlock()
	for _, id := range lost {
		b.replyError(id, why)
	}
}

// disconnect closes the live connection, if any.
func (b *Bridge) disconnect() {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn != nil {
		b.drop(conn, errLost)
	}
}

// drain waits (bounded) for the replies still owed to the client.
func (b *Bridge) drain(ctx context.Context) {
	b.mu.Lock()
	idle := b.idle
	b.mu.Unlock()
	timer := time.NewTimer(b.timing.drain)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// markBusy notes pending is non-empty. Callers hold b.mu.
func (b *Bridge) markBusy() {
	select {
	case <-b.idle:
		b.idle = make(chan struct{})
	default:
	}
}

// markIdleIfDone signals drain once pending is empty. Callers hold b.mu.
func (b *Bridge) markIdleIfDone() {
	if len(b.pending) > 0 {
		return
	}
	select {
	case <-b.idle:
	default:
		close(b.idle)
	}
}

// replyError answers the client's request id with a JSON-RPC error.
func (b *Bridge) replyError(id json.RawMessage, text string) {
	reply, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: errCodeUnavailable, Message: text}})
	if err != nil {
		return
	}
	b.write(append(reply, '\n'))
}

// write sends one message line to the client.
func (b *Bridge) write(line []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.Out.Write(line)
}

// idKey normalizes a JSON-RPC id so the client's spelling and the daemon's
// re-encoding of it compare equal.
func idKey(id json.RawMessage) string {
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return string(id)
	}
	key, err := json.Marshal(v)
	if err != nil {
		return string(id)
	}
	return string(key)
}
