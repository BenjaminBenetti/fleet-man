package podwire

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// A relay stream is one connection a process in the pod made to an in-pod
// socket (TagSockControl, TagSockAgent, TagSockMCP, a host-socket bind's
// TagSockHost) or to a mirrored gateway port (TagHostDial). The agent opens
// it with a SockOpen preamble; fleetd answers with OPEN (SockReply), and from
// then on the connection's bytes flow raw both ways, a FIN ending one
// direction (a half-close) as the end of input does on the in-pod
// connection.

// SockOpen is a relay stream's preamble header.
type SockOpen struct {
	// Peer is the connecting process, as the kernel reported it
	// (SO_PEERCRED): for the audit line only. Who is served is fleetd's
	// decision, from the verified session the stream arrived on.
	Peer *Peer `json:"peer,omitempty"`
	// Target is the host-socket bind a TagSockHost stream's connection was
	// made to (a HostSocket's Target): fleetd serves it only when it recorded
	// that target for the instance, and dials only the path it recorded.
	Target string `json:"target,omitempty"`
	// Port is the gateway port a TagHostDial stream's connection was made to
	// (on HostGatewayAddr): fleetd serves it only while the daemon host
	// listens there.
	Port uint16 `json:"port,omitempty"`
}

// Peer is a process's credentials.
type Peer struct {
	PID int `json:"pid"`
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// SockReply is fleetd's OPEN on a relay stream: empty accepts it, Refused
// and Error say why not. It has the shape of the Mux's own Refusal (a busy or
// unserved kind), so the agent reads both alike.
type SockReply = Refusal

// RefusedGate: fleetd does not serve the socket for the pod now (no TUI is
// attached, agent forwarding is off, the fleet's Fleet MCP is off), or never
// (a host socket or port it did not record or find for the instance).
const RefusedGate = "gate"

// RefusedUnreachable: fleetd could not reach what the stream asks for on the
// daemon host (the recorded socket, the port); its log says why.
const RefusedUnreachable = "unreachable"

// SockRefusedError is a relay stream fleetd refused (or, Agent set, one of
// fleetd's the agent refused: OpenRelay).
type SockRefusedError struct {
	Tag    Tag
	Reason string
	Msg    string
	Agent  bool
}

func (e *SockRefusedError) Error() string {
	msg := e.Msg
	if msg == "" {
		msg = e.Reason
	}
	who := "fleetd"
	if e.Agent {
		who = "the pod agent"
	}
	return fmt.Sprintf("%s refused the %s stream: %s", who, e.Tag, msg)
}

// AcceptSock accepts a relay stream and returns the in-pod connection:
// CloseWrite ends fleetd's direction (the agent half-closes the in-pod
// connection; its input still arrives), Close ends both at once.
func AcceptSock(s *Stream) (net.Conn, error) {
	if err := s.W.WriteJSON(FrameOpen, SockReply{}); err != nil {
		return nil, fmt.Errorf("accept the %s stream: %w", s.Tag, err)
	}
	return &sockConn{Conn: s.Conn}, nil
}

// RefuseSock refuses a relay stream: reason (RefusedGate) and why, for the
// agent.
func RefuseSock(s *Stream, reason, msg string) {
	_ = s.W.WriteJSON(FrameOpen, SockReply{Refused: reason, Error: msg})
}

// sockAnswerWait bounds the agent's wait for fleetd's answer to a relay
// stream.
const sockAnswerWait = 2 * HeaderDeadline

// DialSock opens a relay stream of kind tag on sess (the agent's side of the
// session) and waits for fleetd's answer: the connection to relay the in-pod
// one over, or a SockRefusedError.
func DialSock(sess *yamux.Session, tag Tag, open SockOpen) (net.Conn, error) {
	st, err := sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open a %s stream: %w", tag, err)
	}
	if err := WritePreamble(st, tag, open); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("open a %s stream: %w", tag, err)
	}
	_ = st.SetReadDeadline(time.Now().Add(sockAnswerWait))
	f, err := NewFrameReader(st).Next()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("fleetd's answer to a %s stream: %w", tag, noEOF(err))
	}
	if f.Type != FrameOpen {
		_ = st.Close()
		return nil, fmt.Errorf("fleetd answered a %s stream with %s", tag, f.Type)
	}
	var r SockReply
	if err := DecodeFrame(f, &r); err != nil {
		_ = st.Close()
		return nil, err
	}
	if r.Refused != "" || r.Error != "" {
		_ = st.Close()
		return nil, &SockRefusedError{Tag: tag, Reason: r.Refused, Msg: r.Error}
	}
	_ = st.SetReadDeadline(time.Time{})
	return &sockConn{Conn: st}, nil
}

// IsSockRefused reports whether err is a relay stream fleetd refused with
// reason ("" for any reason).
func IsSockRefused(err error, reason string) bool {
	var re *SockRefusedError
	return errors.As(err, &re) && (reason == "" || re.Reason == reason)
}

// sockConn is a relay stream past its answer. A yamux stream's Close is a
// half-close (its reads go on until the other side's FIN), so CloseWrite is
// that, and Close also ends the reads, which the other side may never end.
type sockConn struct {
	net.Conn
	once sync.Once
}

func (c *sockConn) CloseWrite() error { return c.Conn.Close() }

func (c *sockConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { _ = c.Conn.SetReadDeadline(time.Now()) })
	return err
}
