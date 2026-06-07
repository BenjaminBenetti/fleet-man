package tunnel

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := RegisterRequest{SessionID: "abc123", ClientVersion: "v1.2.3"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out RegisterRequest
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestFrameReplyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := RegisterReply{SessionID: "s1", PublicURL: "https://gw/mcp/s1"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out RegisterReply
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestReadFrameRejectsOversizeHeader guards the allocation cap: a hostile length
// prefix must be rejected before allocating, without reading the body.
func TestReadFrameRejectsOversizeHeader(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrameSize+1)
	r := io.MultiReader(bytes.NewReader(hdr[:]), strings.NewReader("ignored"))
	var v RegisterReply
	if err := ReadFrame(r, &v); err != ErrFrameTooLarge {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestWriteFrameRejectsOversizePayload(t *testing.T) {
	big := RegisterReply{Error: strings.Repeat("x", MaxFrameSize+1)}
	if err := WriteFrame(io.Discard, big); err != ErrFrameTooLarge {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

// TestYamuxSessionOverPipe sanity-checks the session helpers: a client/server
// pair over net.Pipe can open a stream and exchange bytes both directions.
func TestYamuxSessionOverPipe(t *testing.T) {
	c1, c2 := net.Pipe()
	client, err := ClientSession(c1, io.Discard)
	if err != nil {
		t.Fatalf("ClientSession: %v", err)
	}
	defer client.Close()
	server, err := ServerSession(c2, io.Discard)
	if err != nil {
		t.Fatalf("ServerSession: %v", err)
	}
	defer server.Close()

	go func() {
		stream, err := server.Accept()
		if err != nil {
			return
		}
		defer stream.Close()
		b := make([]byte, 4)
		if _, err := io.ReadFull(stream, b); err != nil {
			return
		}
		_, _ = stream.Write(append(b, '!'))
	}()

	stream, err := client.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != "ping!" {
		t.Fatalf("got %q want %q", got, "ping!")
	}
}
