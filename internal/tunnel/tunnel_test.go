package tunnel

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := RegisterRequest{SessionID: "abc123", ClientVersion: "v1.2.3", Features: []string{FeatureGRPC}}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out RegisterRequest
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestFrameReplyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := RegisterReply{SessionID: "s1", PublicURL: "https://gw/mcp/s1", Features: []string{FeatureGRPC}}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out RegisterReply
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestTagRoundTrip(t *testing.T) {
	for _, tag := range []byte{TagMCP, TagGRPC, 0x7f} {
		var buf bytes.Buffer
		if err := WriteTag(&buf, tag); err != nil {
			t.Fatalf("WriteTag(%#x): %v", tag, err)
		}
		got, err := ReadTag(&buf)
		if err != nil {
			t.Fatalf("ReadTag(%#x): %v", tag, err)
		}
		if got != tag {
			t.Fatalf("tag round-trip: got %#x want %#x", got, tag)
		}
	}
}

func TestReadTagShortRead(t *testing.T) {
	if _, err := ReadTag(bytes.NewReader(nil)); err == nil {
		t.Fatal("ReadTag on empty reader should error")
	}
}

func TestNegotiate(t *testing.T) {
	if got := Negotiate([]string{FeatureGRPC, "future"}, []string{FeatureGRPC}); !reflect.DeepEqual(got, []string{FeatureGRPC}) {
		t.Fatalf("Negotiate intersection = %v, want [grpc]", got)
	}
	if got := Negotiate([]string{FeatureGRPC}, nil); len(got) != 0 {
		t.Fatalf("Negotiate with no support = %v, want empty", got)
	}
	if !HasFeature([]string{"a", FeatureGRPC}, FeatureGRPC) || HasFeature([]string{"a"}, FeatureGRPC) {
		t.Fatal("HasFeature wrong")
	}
}

// TestRegisterBackwardCompat confirms an old peer's frame (no "features" key)
// decodes to an absent feature set — so a version-skewed tunnel stays MCP-only.
func TestRegisterBackwardCompat(t *testing.T) {
	var req RegisterRequest
	if err := ReadFrame(frameOf(t, `{"session_id":"s","client_version":"v1"}`), &req); err != nil {
		t.Fatalf("decode legacy request: %v", err)
	}
	if len(req.Features) != 0 {
		t.Fatalf("legacy request should have no features, got %v", req.Features)
	}
	var reply RegisterReply
	if err := ReadFrame(frameOf(t, `{"session_id":"s","public_url":"https://gw/mcp/s"}`), &reply); err != nil {
		t.Fatalf("decode legacy reply: %v", err)
	}
	if HasFeature(reply.Features, FeatureGRPC) {
		t.Fatal("legacy reply must not advertise grpc")
	}
}

// frameOf wraps raw JSON in the length-prefixed frame format for ReadFrame.
func frameOf(t *testing.T, jsonBody string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(jsonBody)))
	buf.Write(hdr[:])
	buf.WriteString(jsonBody)
	return &buf
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
