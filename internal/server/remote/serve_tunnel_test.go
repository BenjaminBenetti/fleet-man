package remote

import (
	"bufio"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
)

// TestServeTunnelDemux is the core fleetd-side test: with FeatureGRPC negotiated,
// every yamux stream is TAG-prefixed, and serveTunnel must route TagMCP streams to
// the MCP reverse proxy and TagGRPC streams (bytes intact after the tag) to the
// gRPC listener — without one stream type disturbing the other, and closing
// unknown-tag streams without wedging the loop.
func TestServeTunnelDemux(t *testing.T) {
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	fleetdConn, gwConn := tcpPair(t)
	fleetdSession, err := tunnel.ClientSession(fleetdConn, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	defer fleetdSession.Close()
	gwSession, err := tunnel.ServerSession(gwConn, io.Discard)
	if err != nil {
		t.Fatalf("server session: %v", err)
	}
	defer gwSession.Close()

	grpcLis := NewChanListener()
	defer grpcLis.Close()
	go func() { _ = serveTunnel(fleetdSession, mcp.port(t), grpcLis, true) }()

	// --- TagGRPC: the post-tag bytes must arrive intact on grpcLis. ---
	gstream, err := gwSession.Open()
	if err != nil {
		t.Fatalf("open grpc stream: %v", err)
	}
	if err := tunnel.WriteTag(gstream, tunnel.TagGRPC); err != nil {
		t.Fatalf("write grpc tag: %v", err)
	}
	if _, err := io.WriteString(gstream, "hello-grpc"); err != nil {
		t.Fatalf("write grpc bytes: %v", err)
	}
	got := make(chan string, 1)
	go func() {
		c, err := grpcLis.Accept()
		if err != nil {
			got <- "ERR:" + err.Error()
			return
		}
		b := make([]byte, len("hello-grpc"))
		if _, err := io.ReadFull(c, b); err != nil {
			got <- "READ:" + err.Error()
			return
		}
		got <- string(b)
	}()
	select {
	case s := <-got:
		if s != "hello-grpc" {
			t.Fatalf("grpc stream payload = %q, want hello-grpc", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("grpc-tagged stream never reached the gRPC listener")
	}

	// --- TagMCP: an HTTP request must reach the MCP reverse proxy, auth intact. ---
	if body := mcpRoundTrip(t, gwSession, "Bearer demux-test"); body != "Bearer demux-test" {
		t.Fatalf("mcp echo through demux = %q", body)
	}

	// --- Unknown tag: the stream is closed and the loop survives. ---
	ustream, err := gwSession.Open()
	if err != nil {
		t.Fatalf("open unknown stream: %v", err)
	}
	_ = tunnel.WriteTag(ustream, 0x42)
	_ = ustream.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := ustream.Read(make([]byte, 1)); err == nil {
		t.Fatal("unknown-tag stream should be closed by fleetd")
	}

	// The loop still serves MCP after the bad stream.
	if body := mcpRoundTrip(t, gwSession, "Bearer after-bad"); body != "Bearer after-bad" {
		t.Fatalf("mcp after unknown tag = %q", body)
	}
}

// TestServeTunnelRejectsMcpWhenDisabled covers the remote-fleet-only tunnel
// (Enable Remote Fleet on, Enable Remote MCP off): TagGRPC streams are still
// routed to the gRPC listener, but TagMCP streams must be CLOSED unanswered —
// the user did not expose MCP — and the loop must keep serving gRPC after one.
func TestServeTunnelRejectsMcpWhenDisabled(t *testing.T) {
	fleetdConn, gwConn := tcpPair(t)
	fleetdSession, err := tunnel.ClientSession(fleetdConn, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	defer fleetdSession.Close()
	gwSession, err := tunnel.ServerSession(gwConn, io.Discard)
	if err != nil {
		t.Fatalf("server session: %v", err)
	}
	defer gwSession.Close()

	grpcLis := NewChanListener()
	defer grpcLis.Close()
	go func() { _ = serveTunnel(fleetdSession, 0, grpcLis, false) }()

	// --- TagMCP: closed unanswered (no HTTP response, the read just fails). ---
	mstream, err := gwSession.Open()
	if err != nil {
		t.Fatalf("open mcp stream: %v", err)
	}
	if err := tunnel.WriteTag(mstream, tunnel.TagMCP); err != nil {
		t.Fatalf("write mcp tag: %v", err)
	}
	_ = mstream.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := mstream.Read(make([]byte, 1)); err == nil {
		t.Fatal("mcp stream should be closed when remote MCP is disabled")
	}

	// --- TagGRPC: still served after the rejected MCP stream. ---
	gstream, err := gwSession.Open()
	if err != nil {
		t.Fatalf("open grpc stream: %v", err)
	}
	if err := tunnel.WriteTag(gstream, tunnel.TagGRPC); err != nil {
		t.Fatalf("write grpc tag: %v", err)
	}
	if _, err := io.WriteString(gstream, "still-grpc"); err != nil {
		t.Fatalf("write grpc bytes: %v", err)
	}
	got := make(chan string, 1)
	go func() {
		c, err := grpcLis.Accept()
		if err != nil {
			got <- "ERR:" + err.Error()
			return
		}
		b := make([]byte, len("still-grpc"))
		if _, err := io.ReadFull(c, b); err != nil {
			got <- "READ:" + err.Error()
			return
		}
		got <- string(b)
	}()
	select {
	case s := <-got:
		if s != "still-grpc" {
			t.Fatalf("grpc stream payload = %q, want still-grpc", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("grpc-tagged stream never reached the gRPC listener")
	}
}

// mcpRoundTrip opens a TagMCP stream on gw, drives a GET /echo through it, and
// returns the echoed Authorization header.
func mcpRoundTrip(t *testing.T, gw *yamux.Session, auth string) string {
	t.Helper()
	stream, err := gw.Open()
	if err != nil {
		t.Fatalf("open mcp stream: %v", err)
	}
	if err := tunnel.WriteTag(stream, tunnel.TagMCP); err != nil {
		t.Fatalf("write mcp tag: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://tunnel/echo", nil)
	req.Header.Set("Authorization", auth)
	if err := req.Write(stream); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(stream), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
