package fleetclient

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// stubGateway accepts one connection, consumes the HTTP request headers, replies
// with statusLine + CRLFCRLF, then (if echo) echoes subsequent bytes.
func stubGateway(t *testing.T, statusLine string, echo bool) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1024)
		// Read until end of headers.
		got := ""
		for !strings.Contains(got, "\r\n\r\n") {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			got += string(buf[:n])
		}
		_, _ = io.WriteString(c, statusLine+"\r\n\r\n")
		if echo {
			_, _ = io.Copy(c, c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestGatewayHandshakeRoundTrip(t *testing.T) {
	ln := stubGateway(t, "HTTP/1.1 200 Connection Established", true)
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()

	conn, err := gatewayHandshake(raw, "gw", "/grpc/abc")
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if _, err := io.WriteString(conn, "hello-tunnel"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len("hello-tunnel"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "hello-tunnel" {
		t.Fatalf("echo = %q, want hello-tunnel", got)
	}
}

func TestGatewayHandshakeNon200(t *testing.T) {
	ln := stubGateway(t, "HTTP/1.1 404 Not Found", false)
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	if _, err := gatewayHandshake(raw, "gw", "/grpc/abc"); err == nil {
		t.Fatal("non-200 handshake should error")
	}
}

func TestDialGatewayConnBadURL(t *testing.T) {
	ctx := context.Background()
	for _, u := range []string{
		"http://gw/grpc/x", // must be https
		"https://gw",       // missing /grpc/<id> path
		"https://gw/",      // empty path
		"://nope",          // unparseable
	} {
		if _, err := dialGatewayConn(ctx, u); err == nil {
			t.Fatalf("dialGatewayConn(%q) should error", u)
		}
	}
}

func TestSelectEndpointGatewayPrecedence(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "https://gw.example.com/grpc/abc123")
	t.Setenv("FLEET_SERVER", "1.2.3.4:9000") // gateway must win
	t.Setenv("FLEET_TOKEN", "tok")

	ep := selectEndpoint()
	ge, ok := ep.(gatewayEndpoint)
	if !ok {
		t.Fatalf("FLEET_GATEWAY should select a gatewayEndpoint, got %T", ep)
	}
	if ep.IsLocal() {
		t.Fatal("gateway endpoint must not be local (no auto-spawn)")
	}
	if ge.token != "tok" {
		t.Fatalf("token = %q, want tok", ge.token)
	}
	if !strings.Contains(ep.String(), "gw.example.com") || strings.Contains(ep.String(), "tok") {
		t.Fatalf("String() should show the URL without the token: %q", ep.String())
	}
}

func TestGatewayTokenFromEnv(t *testing.T) {
	t.Setenv("FLEET_TOKEN", "  env-token  ")
	if got := gatewayToken(); got != "env-token" {
		t.Fatalf("gatewayToken() = %q, want trimmed env-token", got)
	}
}
