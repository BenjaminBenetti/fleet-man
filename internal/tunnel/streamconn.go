package tunnel

import (
	"net"
	"sync"
	"time"
)

// streamconn.go adapts a gRPC bidirectional stream to a net.Conn, so the existing
// yamux reverse tunnel (ClientSession/ServerSession) can run over a single HTTP/2
// stream instead of a raw TCP connection. This is what lets fleetd register and
// tunnel entirely over the gateway's gRPC port — no dedicated TCP control port —
// making the gateway frontable by an L7 proxy (Traefik/Kubernetes).
//
// It imports no grpc: the stream surface and the gRPC codec interface are matched
// structurally, keeping internal/tunnel a stdlib+yamux leaf.

// RawFrame is an opaque gRPC message; RawCodec passes its bytes through unchanged,
// so a gRPC stream can carry arbitrary bytes (the register handshake frames and
// then the yamux byte stream).
type RawFrame struct{ Payload []byte }

// RawCodec is a passthrough gRPC encoding.Codec — it satisfies that interface
// structurally (Marshal/Unmarshal/Name), so this package needs no grpc import.
// BOTH ends of a stream must use it (e.g. ForceServerCodec on the server and
// ForceCodec on the client) for StreamConn and the gRPC proxy to work.
type RawCodec struct{}

func (RawCodec) Marshal(v any) ([]byte, error) { return v.(*RawFrame).Payload, nil }

func (RawCodec) Unmarshal(data []byte, v any) error {
	// grpc may reuse data after Unmarshal returns, so copy it.
	v.(*RawFrame).Payload = append([]byte(nil), data...)
	return nil
}

func (RawCodec) Name() string { return "fleet-raw" }

// rawStream is the gRPC stream surface StreamConn needs — satisfied by both
// grpc.ClientStream and grpc.ServerStream. Declared structurally so this package
// imports no grpc.
type rawStream interface {
	SendMsg(m any) error
	RecvMsg(m any) error
}

// StreamConn adapts a RawCodec gRPC bidi stream to a net.Conn for yamux. Deadlines
// are no-ops: liveness relies on yamux keepalive (which, on a silent peer, closes
// the session and thus — via closeFn — the stream), and the register handshake is
// bounded separately (see Handshake / ReadRegisterRequest). closeFn tears the
// stream down (cancel the RPC context / close the ClientConn) so a blocked Read or
// Write unblocks.
type StreamConn struct {
	stream    rawStream
	closeFn   func()
	closeOnce sync.Once
	readBuf   []byte // bytes of a partially-consumed inbound frame
}

// NewStreamConn wraps a RawCodec gRPC stream as a net.Conn. closeFn (may be nil)
// runs once on Close to tear down the underlying stream/connection.
func NewStreamConn(stream rawStream, closeFn func()) *StreamConn {
	return &StreamConn{stream: stream, closeFn: closeFn}
}

// Read is called only by yamux's single recv goroutine, so it needs no locking.
func (c *StreamConn) Read(p []byte) (int, error) {
	if len(c.readBuf) == 0 {
		var f RawFrame
		if err := c.stream.RecvMsg(&f); err != nil {
			return 0, err // io.EOF on a clean stream end
		}
		c.readBuf = f.Payload
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

// Write is called only by yamux's single send goroutine. It copies p because grpc
// may hold the buffer past SendMsg while yamux reuses its write buffer.
func (c *StreamConn) Write(p []byte) (int, error) {
	if err := c.stream.SendMsg(&RawFrame{Payload: append([]byte(nil), p...)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *StreamConn) Close() error {
	c.closeOnce.Do(func() {
		if c.closeFn != nil {
			c.closeFn()
		}
	})
	return nil
}

func (c *StreamConn) LocalAddr() net.Addr              { return streamAddr{} }
func (c *StreamConn) RemoteAddr() net.Addr             { return streamAddr{} }
func (c *StreamConn) SetDeadline(time.Time) error      { return nil }
func (c *StreamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *StreamConn) SetWriteDeadline(time.Time) error { return nil }

type streamAddr struct{}

func (streamAddr) Network() string { return "fleet-tunnel" }
func (streamAddr) String() string  { return "fleet-tunnel" }

// Handshake performs fleetd's side of the register exchange on conn within timeout
// (StreamConn deadlines are no-ops, so the exchange is bounded here): it writes req
// and reads the reply, closing conn on timeout so a silent gateway can't hang the
// caller.
func Handshake(conn net.Conn, req RegisterRequest, timeout time.Duration) (RegisterReply, error) {
	type result struct {
		reply RegisterReply
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		if err := WriteFrame(conn, req); err != nil {
			ch <- result{err: err}
			return
		}
		var reply RegisterReply
		err := ReadFrame(conn, &reply)
		ch <- result{reply, err}
	}()
	select {
	case r := <-ch:
		return r.reply, r.err
	case <-time.After(timeout):
		_ = conn.Close() // unblock the goroutine's blocked frame read
		return RegisterReply{}, ErrHandshakeTimeout
	}
}

// ReadRegisterRequest performs the gateway's side: it reads the RegisterRequest
// frame within timeout, closing conn on timeout. The caller writes the reply (a
// quick, unbounded write).
func ReadRegisterRequest(conn net.Conn, timeout time.Duration) (RegisterRequest, error) {
	type result struct {
		req RegisterRequest
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var req RegisterRequest
		err := ReadFrame(conn, &req)
		ch <- result{req, err}
	}()
	select {
	case r := <-ch:
		return r.req, r.err
	case <-time.After(timeout):
		_ = conn.Close()
		return RegisterRequest{}, ErrHandshakeTimeout
	}
}
