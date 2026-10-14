package podwire

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ForwardHeader opens a TagForward stream: one TCP connection to Port on the
// pod's loopback, made by the agent. The agent answers with one OPEN frame
// (OpenReply: Error when it could not connect); after it the stream carries
// the connection's bytes both ways, raw. Each side's FIN is a half-close of
// its direction: the agent shuts the TCP connection's write side when
// fleetd's FIN arrives and keeps relaying the peer's answer, so a forward
// over the agent keeps a true half-close.
type ForwardHeader struct {
	Port int `json:"port"`
}

// Validate checks the header names a port.
func (h ForwardHeader) Validate() error {
	if h.Port < 1 || h.Port > 65535 {
		return fmt.Errorf("forward: invalid port %d", h.Port)
	}
	return nil
}

// ErrForwardRefused is a forward the agent could not connect (nothing listens
// on the port in the pod).
var ErrForwardRefused = errors.New("the pod refused the connection")

// Forward connects to port on the pod's loopback through the agent and
// returns the connection once the agent has made it: a port nothing listens
// on fails here (ErrForwardRefused, with the agent's words). The conn's
// CloseWrite is a half-close (the pod's peer reads EOF and can still answer);
// Close ends it: the agent is told (StreamAbort) to close the pod side too,
// so a peer that keeps sending holds neither the agent's stream nor its
// slot. ctx bounds the open, not the connection.
func (c *Client) Forward(ctx context.Context, port int) (net.Conn, error) {
	h := ForwardHeader{Port: port}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	t, err := c.open(ctx, TagForward, h)
	if err != nil {
		return nil, err
	}
	r := NewFrameReader(t)
	f, err := firstFrame(ctx, t, r)
	if err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("forward to port %d: %w", port, err)
	}
	var reply OpenReply
	if f.Type != FrameOpen || DecodeFrame(f, &reply) != nil {
		_ = t.Close()
		return nil, fmt.Errorf("forward to port %d: unexpected %s frame", port, f.Type)
	}
	switch {
	case reply.Refused != "":
		_ = t.Close()
		return nil, fmt.Errorf("forward to port %d: %w: %s", port, ErrRefused, reply.Error)
	case reply.Error != "":
		_ = t.Close()
		return nil, fmt.Errorf("forward to port %d: %w: %s", port, ErrForwardRefused, reply.Error)
	}
	// The frame reader holds nothing back: what follows the OPEN frame is
	// read from the stream itself.
	return &forwardConn{yamuxTransport: t, c: c}, nil
}

// forwardConn is a forward's connection: its Close tells the agent to end
// the stream as well.
type forwardConn struct {
	*yamuxTransport
	c       *Client
	aborted sync.Once
}

func (f *forwardConn) Close() error {
	f.aborted.Do(func() {
		id := f.StreamID()
		// Not in the caller's way: the control stream may be slow to take
		// it (the abort is a courtesy; the stream's close timeout ends it
		// otherwise).
		go func() {
			if f.c.Err() == nil {
				_ = f.c.ctlW.WriteJSON(FrameData, ControlMessage{Type: MsgAbortStream, Abort: &StreamAbort{Stream: id}})
			}
		}()
	})
	return f.yamuxTransport.Close()
}
