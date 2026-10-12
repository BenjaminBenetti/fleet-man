package podwire

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// headerDeadline is HeaderDeadline, shortened by tests.
var headerDeadline = HeaderDeadline

// BulkWindow is the bulk session's yamux stream window: large transfers need
// more in flight than the agent session's default.
const BulkWindow = 4 << 20

const (
	// AgentWriteTimeout is how long the agent session waits for a frame to
	// go out, or for a keepalive ping's answer, before it takes the peer for
	// dead (yamux ConnectionWriteTimeout).
	AgentWriteTimeout = 10 * time.Second
	// BulkWriteTimeout is the same for the bulk session. A full BulkWindow
	// queued on a lossy link (~120 KB/s at a 150ms round trip with 1% loss)
	// takes far longer than 10s to drain, and a ping waits behind it: with
	// the agent session's timeout a large copy tore the bulk session down
	// ("connection to the pod agent lost"). As the gateway's registry
	// connection (tunnel.RegistryKeepAliveTimeout).
	BulkWriteTimeout = 60 * time.Second
)

// SessionConfig is the yamux config both ends of a role's session use.
func SessionConfig(role Role) *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = KeepAliveInterval
	c.ConnectionWriteTimeout = AgentWriteTimeout
	c.StreamOpenTimeout = 30 * time.Second
	c.LogOutput = io.Discard
	if role == RoleBulk {
		c.MaxStreamWindowSize = BulkWindow
		c.ConnectionWriteTimeout = BulkWriteTimeout
	}
	return c
}

// Refusal is an OPEN frame refusing a stream before its handler ran: its kind
// is not served here, or too many of its kind are open.
type Refusal struct {
	Error   string `json:"error,omitempty"`
	Refused string `json:"refused,omitempty"`
}

// Refusal reasons.
const (
	RefusedUnsupported = "unsupported"
	RefusedBusy        = "busy"
	// RefusedOtherPod: the stream named another pod (ExecHeader.PodUID)
	// than the one the agent runs in; it was replaced.
	RefusedOtherPod = "other_pod"
)

// ErrRefused wraps a refused stream's error.
var ErrRefused = errors.New("stream refused")

// Stream is a stream the other side opened, past its preamble.
type Stream struct {
	Tag Tag
	// ID is the yamux stream's id, the same on both ends.
	ID     uint32
	Header []byte
	Conn   net.Conn
	R      *FrameReader
	W      *FrameWriter

	sess *yamux.Session
	mux  *Mux
}

// TransportLost reports whether the stream's session is gone: after a read
// fails, it tells a dropped transport (hold) from the other side's FIN or
// ABORT (abort).
func (s *Stream) TransportLost() bool { return s.sess != nil && s.sess.IsClosed() }

// Penalize records that the other side broke the protocol on this stream;
// the session then waits before accepting its next stream.
func (s *Stream) Penalize() {
	if s.mux != nil {
		s.mux.penalty.add()
	}
}

// Handler serves one accepted stream; the stream is closed when it returns.
type Handler func(*Stream)

// Mux accepts the streams the other side opens, reads their preambles under
// HeaderDeadline and hands each to its tag's handler, within the tag's cap.
type Mux struct {
	Handlers map[Tag]Handler
	// Refused, when set, hears about each refused or broken stream.
	Refused func(tag Tag, reason string)

	mu      sync.Mutex
	active  map[Tag]int
	penalty penalty
}

// Serve accepts streams until the session ends, and returns why it ended.
func (m *Mux) Serve(sess *yamux.Session) error {
	for {
		m.penalty.wait(sess.CloseChan())
		st, err := sess.AcceptStream()
		if err != nil {
			return err
		}
		go m.serveStream(sess, st)
	}
}

func (m *Mux) serveStream(sess *yamux.Session, st *yamux.Stream) {
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(headerDeadline))
	tag, header, err := ReadPreamble(st)
	if err != nil {
		m.penalty.add()
		m.refused(0, "bad preamble: "+err.Error())
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	w := NewFrameWriter(st)
	handler, ok := m.Handlers[tag]
	if !ok {
		_ = w.WriteJSON(FrameOpen, Refusal{Error: fmt.Sprintf("%s streams are not served here", tag), Refused: RefusedUnsupported})
		m.refused(tag, "unsupported")
		return
	}
	if !m.acquire(tag) {
		_ = w.WriteJSON(FrameOpen, Refusal{Error: fmt.Sprintf("too many %s streams open", tag), Refused: RefusedBusy})
		m.refused(tag, "over the stream cap")
		return
	}
	defer m.release(tag)
	m.penalty.clean()
	handler(&Stream{Tag: tag, ID: st.StreamID(), Header: header, Conn: st, R: NewFrameReader(st), W: w, sess: sess, mux: m})
}

func (m *Mux) refused(tag Tag, reason string) {
	if m.Refused != nil {
		m.Refused(tag, reason)
	}
}

func (m *Mux) acquire(tag Tag) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active = map[Tag]int{}
	}
	limit := StreamCap(tag)
	if limit <= 0 || m.active[tag] >= limit {
		return false
	}
	m.active[tag]++
	return true
}

func (m *Mux) release(tag Tag) {
	m.mu.Lock()
	m.active[tag]--
	m.mu.Unlock()
}

// Active is how many streams of kind tag are being served.
func (m *Mux) Active(tag Tag) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[tag]
}

// penalty backs off accepting streams after the other side broke the
// protocol: each violation doubles the wait before the next accept, from
// penaltyBase up to penaltyMax, and each well-formed stream halves it.
type penalty struct {
	mu sync.Mutex
	d  time.Duration
}

const (
	penaltyBase = 50 * time.Millisecond
	penaltyMax  = 5 * time.Second
)

func (p *penalty) add() {
	p.mu.Lock()
	p.d = min(max(2*p.d, penaltyBase), penaltyMax)
	p.mu.Unlock()
}

func (p *penalty) clean() {
	p.mu.Lock()
	if p.d /= 2; p.d < penaltyBase {
		p.d = 0
	}
	p.mu.Unlock()
}

func (p *penalty) current() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.d
}

func (p *penalty) wait(done <-chan struct{}) {
	if d := p.current(); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-done:
		}
	}
}
