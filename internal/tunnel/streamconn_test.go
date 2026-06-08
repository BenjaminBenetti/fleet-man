package tunnel

import (
	"errors"
	"io"
	"testing"
	"time"
)

// memStream is an in-memory rawStream: SendMsg pushes a frame's bytes to out,
// RecvMsg pops one frame from in (blocking; io.EOF when in is closed).
type memStream struct {
	in  chan []byte
	out chan []byte
}

func (m *memStream) SendMsg(v any) error {
	m.out <- append([]byte(nil), v.(*RawFrame).Payload...)
	return nil
}

func (m *memStream) RecvMsg(v any) error {
	b, ok := <-m.in
	if !ok {
		return io.EOF
	}
	v.(*RawFrame).Payload = b
	return nil
}

// TestStreamConnPartialReads verifies Read drains a frame larger than the caller's
// buffer across multiple calls (the readBuf path).
func TestStreamConnPartialReads(t *testing.T) {
	m := &memStream{in: make(chan []byte, 1)}
	conn := NewStreamConn(m, nil)
	m.in <- []byte("abcdefgh") // one 8-byte frame
	close(m.in)

	p := make([]byte, 3)
	var got []byte
	for len(got) < 8 {
		n, err := conn.Read(p)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, p[:n]...)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("reassembled %q, want abcdefgh", got)
	}
}

// TestStreamConnWrite verifies each Write becomes one frame on the stream.
func TestStreamConnWrite(t *testing.T) {
	m := &memStream{out: make(chan []byte, 1)}
	conn := NewStreamConn(m, nil)
	n, err := conn.Write([]byte("xyz"))
	if err != nil || n != 3 {
		t.Fatalf("write = (%d, %v), want (3, nil)", n, err)
	}
	if got := <-m.out; string(got) != "xyz" {
		t.Fatalf("frame = %q, want xyz", got)
	}
}

// TestStreamConnFrameRoundTrip verifies a WriteFrame on one StreamConn is read back
// intact by ReadFrame on another — i.e. WriteFrame's two writes (header + body)
// become two frames that ReadFrame's two io.ReadFull calls reassemble across frame
// boundaries via readBuf.
func TestStreamConnFrameRoundTrip(t *testing.T) {
	pipe := make(chan []byte, 8)
	writer := NewStreamConn(&memStream{out: pipe}, nil)
	reader := NewStreamConn(&memStream{in: pipe}, nil)

	want := RegisterRequest{SessionID: "s1", ClientVersion: "v", Features: []string{FeatureGRPC}}
	go func() { _ = WriteFrame(writer, want) }()

	var got RegisterRequest
	if err := ReadFrame(reader, &got); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if got.SessionID != "s1" || got.ClientVersion != "v" || !HasFeature(got.Features, FeatureGRPC) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestHandshakeTimeout verifies Handshake bounds itself and closes the conn (so the
// blocked frame read unblocks) when the gateway never replies.
func TestHandshakeTimeout(t *testing.T) {
	m := &memStream{in: make(chan []byte), out: make(chan []byte, 4)} // in is never fed
	closed := false
	conn := NewStreamConn(m, func() { closed = true; close(m.in) })

	_, err := Handshake(conn, RegisterRequest{}, 50*time.Millisecond)
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Fatalf("Handshake err = %v, want ErrHandshakeTimeout", err)
	}
	if !closed {
		t.Fatal("Handshake should close the conn on timeout")
	}
}

// TestReadRegisterRequestTimeout verifies the gateway-side bounded read times out
// when the client never sends a RegisterRequest.
func TestReadRegisterRequestTimeout(t *testing.T) {
	m := &memStream{in: make(chan []byte)} // never fed
	conn := NewStreamConn(m, func() { close(m.in) })
	_, err := ReadRegisterRequest(conn, 50*time.Millisecond)
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Fatalf("ReadRegisterRequest err = %v, want ErrHandshakeTimeout", err)
	}
}
