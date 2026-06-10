package portforward

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// tcpTarget returns a TargetDialer that opens plain TCP connections to addr.
func tcpTarget(addr string) TargetDialer {
	return func() (io.ReadWriteCloser, error) {
		return net.Dial("tcp", addr)
	}
}

// startEcho runs a TCP echo server and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func dialRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestManagerAddProxiesAndRemoves(t *testing.T) {
	echo := startEcho(t)
	mgr := NewManager()
	defer mgr.Shutdown()

	localPort := freePort(t)
	if err := mgr.Add("f/i", localPort, 80, tcpTarget(echo)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	conn := dialRetry(t, addr)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q err=%v", buf, err)
	}
	conn.Close()

	if got := mgr.FormatLabels("f/i"); got != fmt.Sprintf("%d->80", localPort) {
		t.Fatalf("labels: %q", got)
	}

	if err := mgr.Remove("f/i", localPort); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// The port is free again: a fresh forward can bind it.
	if err := mgr.Add("f/i", localPort, 80, tcpTarget(echo)); err != nil {
		t.Fatalf("re-Add after Remove: %v", err)
	}
}

// TestStopForwardClosesLiveConnections: removing a forward kills established
// tunnels, not just the listener — that's what tears down the per-connection
// server bridges.
func TestStopForwardClosesLiveConnections(t *testing.T) {
	echo := startEcho(t)
	mgr := NewManager()
	defer mgr.Shutdown()

	localPort := freePort(t)
	if err := mgr.Add("f/i", localPort, 80, tcpTarget(echo)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	conn := dialRetry(t, fmt.Sprintf("127.0.0.1:%d", localPort))
	defer conn.Close()
	// Prove the tunnel is live before removing.
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(conn, one); err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := mgr.Remove("f/i", localPort); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The established connection is closed out from under the client.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(one); err == nil {
		t.Fatalf("expected read to fail after Remove")
	}
}

func TestAddBrowserProxyPicksPortAndIsFindable(t *testing.T) {
	echo := startEcho(t)
	mgr := NewManager()
	defer mgr.Shutdown()

	port, err := mgr.AddBrowserProxy("f/i", 58888, tcpTarget(echo))
	if err != nil {
		t.Fatalf("AddBrowserProxy: %v", err)
	}
	if found, ok := mgr.FindBrowserProxy("f/i"); !ok || found != port {
		t.Fatalf("FindBrowserProxy: %d %v want %d", found, ok, port)
	}
	if got := mgr.FormatLabels("f/i"); got != fmt.Sprintf("%d->proxy", port) {
		t.Fatalf("labels: %q", got)
	}
}
