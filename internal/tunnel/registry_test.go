package tunnel

import (
	"bytes"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// TestTagRegistryIsDistinct pins TagRegistry to 0x03, the next free tag, so a
// registry stream can never be mistaken for MCP, gRPC or webhook traffic.
func TestTagRegistryIsDistinct(t *testing.T) {
	if TagRegistry != 0x03 {
		t.Fatalf("TagRegistry = %#x, want 0x03", TagRegistry)
	}
	seen := map[byte]bool{}
	for _, tag := range []byte{TagMCP, TagGRPC, TagWebhook, TagRegistry} {
		if seen[tag] {
			t.Fatalf("tag %#x is used twice", tag)
		}
		seen[tag] = true
	}
}

// TestTagged is the shared wire predicate: any one tagging feature switches the
// tunnel to tagged streams, and a set without one (legacy, or unknown future
// features only) stays untagged.
func TestTagged(t *testing.T) {
	cases := []struct {
		features []string
		want     bool
	}{
		{nil, false},
		{[]string{"future"}, false},
		{[]string{FeatureGRPC}, true},
		{[]string{FeatureWebhook}, true},
		{[]string{FeatureRegistry}, true},
		{[]string{"future", FeatureRegistry}, true},
		{[]string{FeatureGRPC, FeatureWebhook, FeatureRegistry}, true},
	}
	for _, c := range cases {
		if got := Tagged(c.features); got != c.want {
			t.Errorf("Tagged(%v) = %v, want %v", c.features, got, c.want)
		}
	}
}

// TestRegistryFieldsRoundTrip covers the attach handshake and the registry
// reply field on the wire, and that a peer predating them decodes to empty
// values (a registration, no registry).
func TestRegistryFieldsRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := RegisterRequest{SessionID: "s", Attach: AttachRegistry}
	if err := WriteFrame(&buf, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var gotReq RegisterRequest
	if err := ReadFrame(&buf, &gotReq); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !reflect.DeepEqual(gotReq, req) {
		t.Fatalf("request round trip: got %+v want %+v", gotReq, req)
	}

	reply := RegisterReply{SessionID: "s", PublicRegistry: "gw.example.com/rid", Features: []string{FeatureRegistry}}
	off := RegisterReply{SessionID: "s", RegistryOff: "no --session-key"}
	if err := WriteFrame(&buf, off); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var gotOff RegisterReply
	if err := ReadFrame(&buf, &gotOff); err != nil || !reflect.DeepEqual(gotOff, off) {
		t.Fatalf("reply round trip: got %+v (%v) want %+v", gotOff, err, off)
	}
	if err := WriteFrame(&buf, reply); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var gotReply RegisterReply
	if err := ReadFrame(&buf, &gotReply); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !reflect.DeepEqual(gotReply, reply) {
		t.Fatalf("reply round trip: got %+v want %+v", gotReply, reply)
	}

	var legacy RegisterRequest
	if err := ReadFrame(frameOf(t, `{"session_id":"s"}`), &legacy); err != nil {
		t.Fatalf("decode legacy request: %v", err)
	}
	if legacy.Attach != "" {
		t.Fatalf("a legacy request must decode as a registration, got attach %q", legacy.Attach)
	}
}

// TestRegistrySessionConfig pins the registry connection's yamux tuning, and
// that the main tunnel keeps yamux's defaults.
func TestRegistrySessionConfig(t *testing.T) {
	reg := registrySessionConfig(io.Discard)
	if reg.MaxStreamWindowSize != 4<<20 {
		t.Fatalf("registry stream window = %d, want 4 MiB", reg.MaxStreamWindowSize)
	}
	if reg.ConnectionWriteTimeout != 60*time.Second {
		t.Fatalf("registry keepalive timeout = %v, want 60s", reg.ConnectionWriteTimeout)
	}
	if err := yamux.VerifyConfig(reg); err != nil {
		t.Fatalf("registry config rejected by yamux: %v", err)
	}
	main, def := sessionConfig(io.Discard), yamux.DefaultConfig()
	if main.MaxStreamWindowSize != def.MaxStreamWindowSize || main.ConnectionWriteTimeout != def.ConnectionWriteTimeout {
		t.Fatalf("main tunnel config changed: window %d timeout %v", main.MaxStreamWindowSize, main.ConnectionWriteTimeout)
	}
}

// TestRegistrySessionWindow is the behaviour the 4 MiB window buys: on a
// registry connection fleetd can put 3 MiB of a layer in flight on one stream
// before the gateway reads any of it, where the main tunnel's 256 KiB window
// stalls the same write.
func TestRegistrySessionWindow(t *testing.T) {
	const size = 3 << 20
	payload := bytes.Repeat([]byte("layer"), size/5+1)[:size]

	// writeUnread opens a stream from the gateway end, has the fleetd end write
	// the payload, and reports whether the write finished within wait while the
	// gateway end has read nothing. It then drains the stream and checks the
	// bytes arrived intact.
	writeUnread := func(t *testing.T, server, client func(net.Conn, io.Writer) (*yamux.Session, error), wait time.Duration) bool {
		t.Helper()
		c1, c2 := net.Pipe()
		gw, err := server(c1, io.Discard)
		if err != nil {
			t.Fatalf("server session: %v", err)
		}
		defer gw.Close()
		fleetd, err := client(c2, io.Discard)
		if err != nil {
			t.Fatalf("client session: %v", err)
		}
		defer fleetd.Close()

		gwStream, err := gw.Open()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		fleetdStream, err := fleetd.Accept()
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := fleetdStream.Write(payload)
			done <- err
		}()
		finished := false
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			finished = true
		case <-time.After(wait):
		}
		got := make([]byte, size)
		if _, err := io.ReadFull(gwStream, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("payload corrupted in transit")
		}
		if !finished {
			if err := <-done; err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		return finished
	}

	if !writeUnread(t, RegistryServerSession, RegistryClientSession, 5*time.Second) {
		t.Fatal("a 3 MiB write on a registry connection stalled before the gateway read")
	}
	if writeUnread(t, ServerSession, ClientSession, 300*time.Millisecond) {
		t.Fatal("a 3 MiB write on the main tunnel finished unread; the window test proves nothing")
	}
}
