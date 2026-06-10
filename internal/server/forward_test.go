package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedForwardInstance writes one running instance to the isolated state dir.
func seedForwardInstance(t *testing.T) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendDevcontainer, WorkspaceDir: "/ws/alpha/i1", ContainerID: "c1", Status: fleet.StatusRunning},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// startEchoServer runs a TCP echo server (io.Copy(conn, conn)) and returns
// its address. An echo handles half-close naturally: client CloseWrite ->
// read EOF -> copy returns -> conn closes -> client read EOF.
func startEchoServer(t *testing.T) string {
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

// stubBridgeTo points openForwardBridge at a plain TCP dial of addr for the
// duration of the test, recording the open parameters it saw.
func stubBridgeTo(t *testing.T, addr string) *struct {
	Fleet, Instance string
	RemotePort      int
} {
	t.Helper()
	seen := &struct {
		Fleet, Instance string
		RemotePort      int
	}{}
	orig := openForwardBridge
	openForwardBridge = func(inst *fleet.Instance, remotePort int) (io.ReadWriteCloser, error) {
		seen.Instance = inst.Name
		seen.RemotePort = remotePort
		return net.Dial("tcp", addr)
	}
	t.Cleanup(func() { openForwardBridge = orig })
	return seen
}

func forwardOpenChunk(fleetName, instance string, remotePort int) *fleetgrpc.ForwardChunk {
	return &fleetgrpc.ForwardChunk{Msg: &fleetgrpc.ForwardChunk_Open{Open: &fleetgrpc.ForwardOpen{
		Fleet:      fleetName,
		Instance:   instance,
		RemotePort: int32(remotePort),
	}}}
}

func forwardDataChunk(data []byte) *fleetgrpc.ForwardChunk {
	return &fleetgrpc.ForwardChunk{Msg: &fleetgrpc.ForwardChunk_Data{Data: data}}
}

// TestForwardRoundTrip drives the Forward stream directly: open, write,
// half-close, and expect the echoed bytes followed by a clean stream end.
func TestForwardRoundTrip(t *testing.T) {
	isolateFleetDir(t)
	seedForwardInstance(t)
	seen := stubBridgeTo(t, startEchoServer(t))

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if err := stream.Send(forwardOpenChunk("alpha", "i1", 8080)); err != nil {
		t.Fatalf("send open: %v", err)
	}
	payload := []byte("hello over the data plane")
	if err := stream.Send(forwardDataChunk(payload)); err != nil {
		t.Fatalf("send data: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var got bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got.Write(chunk.GetData())
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("echo mismatch: got %q want %q", got.Bytes(), payload)
	}
	if seen.Instance != "i1" || seen.RemotePort != 8080 {
		t.Fatalf("bridge saw wrong target: %+v", seen)
	}
}

// TestForwardRejectsBadFirstFrame: a data frame before open is an
// InvalidArgument, and a bogus remote port is too.
func TestForwardRejectsBadFirstFrame(t *testing.T) {
	isolateFleetDir(t)
	seedForwardInstance(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if err := stream.Send(forwardDataChunk([]byte("no header"))); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}

	stream, err = client.Forward(ctx)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if err := stream.Send(forwardOpenChunk("alpha", "i1", 0)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for port 0, got %v", err)
	}
}

// TestForwardUnknownInstanceIsNotFound: a typo'd target fails fast.
func TestForwardUnknownInstanceIsNotFound(t *testing.T) {
	isolateFleetDir(t)
	seedForwardInstance(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Forward(ctx)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if err := stream.Send(forwardOpenChunk("alpha", "ghost", 80)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TestForwardEndToEndThroughManager exercises the full client path: a
// portforward.Manager listener whose connections tunnel over NewGRPCTarget
// streams to the (stubbed) server bridge — i.e. exactly what the TUI/CLI run.
func TestForwardEndToEndThroughManager(t *testing.T) {
	isolateFleetDir(t)
	seedForwardInstance(t)
	stubBridgeTo(t, startEchoServer(t))

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	localPort := freeTestPort(t)
	mgr := portforward.NewManager()
	defer mgr.Shutdown()
	dial := portforward.NewGRPCTarget(client, "alpha", "i1", 9090)
	if err := mgr.Add("alpha/i1", localPort, 9090, dial); err != nil {
		t.Fatalf("Add: %v", err)
	}

	conn := dialWithRetry(t, fmt.Sprintf("127.0.0.1:%d", localPort))
	defer conn.Close()

	payload := strings.Repeat("fleet-man forward e2e ", 2048) // ~45 KiB: spans multiple chunks
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Half-close the upload so the echo bridge sees EOF and finishes.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("echo mismatch: got %d bytes want %d", len(got), len(payload))
	}

	// Duplicate local port is rejected while the forward is up.
	if err := mgr.Add("alpha/i1", localPort, 9090, dial); err == nil {
		t.Fatalf("expected duplicate-port error")
	}
}

// freeTestPort grabs an ephemeral port and releases it for the code under
// test to bind.
func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// dialWithRetry dials addr, retrying briefly while the proxy listener comes
// up.
func dialWithRetry(t *testing.T, addr string) net.Conn {
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
