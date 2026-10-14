package sharesync

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// remote.go is the wire between the hub and a side in another process: a
// pod's spoke, which runs a Tree over the fleet's share (or an instance's
// own copy) and reaches fleetd over the pod agent's sessions. The two ends
// exchange the same structs the daemon host's own side passes in process,
// as length-prefixed JSON messages on one control connection; content over
// InlineMax travels on a connection of its own (BulkConn), so a large file
// never queues in front of a login.
//
// The hub's end (ServeRemote) takes nothing the spoke says about who it is:
// the caller names the side (SideInfo, from its own record of the pod), and
// the spoke's hello and reports are bounded before the hub sees them (the
// paths it may claim, the revs: Attach adopts a held rev above the hub's
// own, so an absurd one would wrap the root's).

// InlineMax is the most content that travels inside the control connection.
const InlineMax = 256 << 10

// maxWireMessage bounds one message on the wire.
const maxWireMessage = 8 << 20

// helloChunk is how many held paths one hello message carries.
const helloChunk = 4096

// maxReportChanges bounds the changes of one reported message (a side sends
// at most SideOptions.ReportSize, 512 by default).
const maxReportChanges = 4096

// MaxRemoteHeld bounds the paths a spoke's hello may name: well past any
// real share (agents' transcripts run to tens of thousands of files), and
// what fleetd holds in memory for a hostile pod's hello stays bounded too.
const MaxRemoteHeld = 1 << 20

// maxRemoteCalls bounds the reports, and the content asks, of one spoke the
// hub serves at once; one past it is refused (the spoke tries again on its
// next scan, or its op again later). A spoke scans one root at a time, so
// it has at most a report or two in flight.
const (
	maxRemoteReports = 16
	maxRemoteNeeds   = 64
)

// maxRemoteRev bounds a rev a spoke may claim: revs count accepted changes,
// and 2^53 is beyond any real one.
const maxRemoteRev = 1 << 53

// maxEpoch bounds an epoch string from a spoke (the hub's are 32 hex).
const maxEpoch = 64

// Message types. Requests carry an ID their answer repeats; answers are
// only ever matched against the receiver's own calls.
const (
	msgHello   = "hello"   // spoke: what it holds (chunked; More on all but the last)
	msgWelcome = "welcome" // hub: the epochs
	msgRefused = "refused" // hub: the spoke is not attached (Err says why)
	msgReport  = "report"  // spoke: changes in one root
	msgReply   = "reply"   // hub: the acks
	msgApply   = "apply"   // hub: a batch to apply
	msgResults = "results" // spoke: how applying it went
	msgFetch   = "fetch"   // hub: content the spoke holds
	msgNeed    = "need"    // spoke: content of an op of a batch it is applying
	msgData    = "data"    // either: content inline, or Bulk on a connection of its own
	msgRescan  = "rescan"  // hub: scan and report now
	msgHint    = "hint"    // hub: paths changed on another node (no answer)
	msgDone    = "done"    // spoke: the rescan finished
	msgError   = "error"   // either: the request failed (Stale for ErrStale)
)

// answers are the message types that answer a call.
var answers = map[string]bool{msgWelcome: true, msgRefused: true, msgReply: true, msgResults: true, msgData: true, msgDone: true, msgError: true}

// wireMsg is one message.
type wireMsg struct {
	T       string    `json:"t"`
	ID      uint64    `json:"id,omitempty"`
	Hello   *Hello    `json:"hello,omitempty"`
	More    bool      `json:"more,omitempty"`
	Lease   uint64    `json:"lease,omitempty"`
	Welcome *Welcome  `json:"welcome,omitempty"`
	Report  *Report   `json:"report,omitempty"`
	Reply   *Reply    `json:"reply,omitempty"`
	Batch   *Batch    `json:"batch,omitempty"`
	Results []Result  `json:"results,omitempty"`
	Get     *wireGet  `json:"get,omitempty"`
	Need    *wireNeed `json:"need,omitempty"`
	Data    []byte    `json:"data,omitempty"`
	Bulk    bool      `json:"bulk,omitempty"`
	Err     string    `json:"err,omitempty"`
	Stale   bool      `json:"stale,omitempty"`
	Hint    *Hint     `json:"hint,omitempty"`
}

// wireGet names content a side holds.
type wireGet struct {
	Root   string `json:"root"`
	Path   string `json:"p"`
	Entry  Entry  `json:"e"`
	Offset int64  `json:"off,omitempty"`
}

// wireNeed is a spoke's ask for an op's content: Batch is the apply call's
// ID.
type wireNeed struct {
	Batch  uint64 `json:"batch"`
	Op     Op     `json:"op"`
	Offset int64  `json:"off,omitempty"`
}

// The kinds of a bulk connection (BulkHeader.Kind).
const (
	// BulkPut carries the hub's content for the spoke's need Token.
	BulkPut = "put"
	// BulkGet asks the spoke for content (Get); the spoke answers a status
	// message, then the content.
	BulkGet = "get"
)

// BulkHeader is a bulk connection's first message.
type BulkHeader struct {
	Kind  string   `json:"kind"`
	Token uint64   `json:"token,omitempty"`
	Get   *wireGet `json:"get,omitempty"`
}

// writeMsg writes v as one length-prefixed JSON message.
func writeMsg(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > maxWireMessage {
		return fmt.Errorf("sharesync: a %d-byte message is over the wire's %d", len(body), maxWireMessage)
	}
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(body)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// readMsg reads one message written by writeMsg into v.
func readMsg(r io.Reader, v any) error {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > maxWireMessage {
		return fmt.Errorf("sharesync: a %d-byte message is over the wire's %d", n, maxWireMessage)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// errWireClosed is a call on a connection that ended.
var errWireClosed = errors.New("sharesync: the connection to the other side ended")

// wire is one end of a control connection: calls and their answers, both
// ways, on one connection.
type wire struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex

	mu    sync.Mutex
	next  uint64
	calls map[uint64]chan wireMsg
	err   error
	done  chan struct{}
}

func newWire(conn net.Conn) *wire {
	return &wire{conn: conn, r: bufio.NewReaderSize(conn, 64<<10), calls: map[uint64]chan wireMsg{}, done: make(chan struct{})}
}

func (w *wire) send(m wireMsg) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if err := writeMsg(w.conn, m); err != nil {
		w.fail(err)
		return err
	}
	return nil
}

func (w *wire) recv() (wireMsg, error) {
	var m wireMsg
	err := readMsg(w.r, &m)
	return m, err
}

// fail ends the connection: every call waiting is answered with err.
func (w *wire) fail(err error) {
	w.mu.Lock()
	if w.err != nil {
		w.mu.Unlock()
		return
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = errWireClosed
	}
	w.err = err
	calls := w.calls
	w.calls = map[uint64]chan wireMsg{}
	close(w.done)
	w.mu.Unlock()
	_ = w.conn.Close()
	for _, ch := range calls {
		close(ch)
	}
}

// Err is why the connection ended, nil while it runs.
func (w *wire) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// id hands out a call ID.
func (w *wire) id() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	return w.next
}

// call sends m as a request (with id, or a new one when 0) and waits for its
// answer.
func (w *wire) call(ctx context.Context, id uint64, m wireMsg) (wireMsg, error) {
	if id == 0 {
		id = w.id()
	}
	m.ID = id
	ch := make(chan wireMsg, 1)
	w.mu.Lock()
	if w.err != nil {
		err := w.err
		w.mu.Unlock()
		return wireMsg{}, err
	}
	w.calls[id] = ch
	w.mu.Unlock()
	drop := func() {
		w.mu.Lock()
		delete(w.calls, id)
		w.mu.Unlock()
	}
	if err := w.send(m); err != nil {
		drop()
		return wireMsg{}, err
	}
	select {
	case a, ok := <-ch:
		if !ok {
			return wireMsg{}, w.Err()
		}
		if a.T == msgError {
			return a, a.error()
		}
		return a, nil
	case <-ctx.Done():
		drop()
		return wireMsg{}, ctx.Err()
	}
}

// deliver hands an answer to its call; false when no call waits for it.
func (w *wire) deliver(m wireMsg) bool {
	w.mu.Lock()
	ch := w.calls[m.ID]
	delete(w.calls, m.ID)
	w.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- m
	return true
}

// answer answers request id.
func (w *wire) answer(id uint64, m wireMsg) {
	m.ID = id
	_ = w.send(m)
}

// answerErr answers request id with err.
func (w *wire) answerErr(id uint64, err error) {
	w.answer(id, wireMsg{T: msgError, Err: err.Error(), Stale: errors.Is(err, ErrStale)})
}

// error is an error answer as an error.
func (m wireMsg) error() error {
	if m.Stale {
		return fmt.Errorf("%s: %w", m.Err, ErrStale)
	}
	return errors.New(m.Err)
}

// inline reads rc whole when it holds at most InlineMax bytes: data and
// true; otherwise the bytes read so far and false, with rc still open.
func inline(rc io.Reader) ([]byte, bool, error) {
	buf := make([]byte, 0, 32<<10)
	lim := io.LimitReader(rc, InlineMax+1)
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := lim.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, len(buf) <= InlineMax, nil
		}
		if err != nil {
			return nil, false, err
		}
		if len(buf) > InlineMax {
			return buf, false, nil
		}
	}
}

// connReader is content arriving on a connection, which closing ends.
type connReader struct {
	io.Reader
	conn net.Conn
	done func()
}

func (c *connReader) Close() error {
	err := c.conn.Close()
	if c.done != nil {
		c.done()
		c.done = nil
	}
	return err
}
