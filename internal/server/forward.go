package server

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// forward.go is the SERVER half of the port-forward data plane (issue #81):
// one Forward bidi stream per forwarded TCP connection. The client owns the
// listen half (internal/portforward); this side reads the ForwardOpen header,
// resolves the instance, opens a byte bridge to remote_port inside the
// container, and pipes raw bytes both ways. Because the bytes traverse the
// gRPC stream, forwarding keeps working when the server is remote — the old
// PortForward descriptor RPC assumed client host == server host.

// forwardChunkSize bounds one data frame. 32 KiB matches io.Copy's default
// buffer; well under gRPC's 4 MB message ceiling.
const forwardChunkSize = 32 * 1024

// forwarderDialTimeout bounds how long openForwardBridge waits for a spawned
// backend forwarder (coder/gh) to start accepting on its host-local port.
// Generous: both CLIs do a network handshake before listening.
const forwarderDialTimeout = 15 * time.Second

// closeWriter is the optional half-close half of a bridge: shut the write
// side while reads continue draining (a TCP CloseWrite / an EOF on a stdio
// bridge's stdin).
type closeWriter interface{ CloseWrite() error }

// openForwardBridge opens the server-side byte bridge to remotePort inside
// the instance. Package var so tests can stub the whole container layer.
var openForwardBridge = func(inst *fleet.Instance, remotePort int) (io.ReadWriteCloser, error) {
	b := backendutil.NewForInstance(inst, false)

	// Fast path: the container is directly reachable from the server host
	// (devcontainer on native Linux) — a plain TCP dial, no subprocess.
	if host, ok := b.ResolveHostname(inst.ContainerID); ok {
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", remotePort))
		if conn, err := net.DialTimeout("tcp", addr, 3*time.Second); err == nil {
			return conn, nil
		}
		// Unreachable (e.g. Docker Desktop's unrouted container IPs) or
		// refused: fall through to the backend bridge, which connects from
		// inside the container's own network namespace.
	}

	// Stdio bridge: one process per connection, stdin/stdout are the pipe
	// (devcontainer: docker exec socat).
	if cmd, ok := b.ForwardStdioCommand(inst.ContainerID, remotePort); ok {
		return startStdioBridge(cmd)
	}

	// Fallback for backends with only a listening forwarder (coder/gh):
	// bind it to a host-local ephemeral port and dial that. The "host" here
	// is the SERVER host, so this stays correct for a remote server.
	return dialViaForwarder(b, inst.ContainerID, remotePort)
}

// Forward implements the port-forward data plane. The first client frame
// must carry the ForwardOpen header; afterwards both directions carry raw
// bytes. The client half-closes its upload via CloseSend; the server signals
// remote EOF by returning (which ends the stream).
func (s *service) Forward(stream fleetgrpc.FleetService_ForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first Forward frame must carry open")
	}
	inst, err := resolveServerInstance(open.GetFleet(), open.GetInstance())
	if err != nil {
		return err
	}
	remotePort := int(open.GetRemotePort())
	if remotePort < 1 || remotePort > 65535 {
		return status.Errorf(codes.InvalidArgument, "invalid remote port %d", remotePort)
	}

	bridge, err := openForwardBridge(inst, remotePort)
	if err != nil {
		flog.Warn("forward: open bridge", "fleet", open.GetFleet(), "instance", open.GetInstance(), "remotePort", remotePort, "err", err)
		return status.Errorf(codes.Unavailable, "open bridge to %s/%s:%d: %v", open.GetFleet(), open.GetInstance(), remotePort, err)
	}
	defer bridge.Close()

	// client -> bridge. A client CloseSend (io.EOF) half-closes the bridge
	// write side so the in-container peer sees EOF but can keep responding;
	// any other recv error means the client is gone, so tear the bridge down
	// to unblock the read pump below.
	go func() {
		for {
			chunk, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					if cw, ok := bridge.(closeWriter); ok {
						_ = cw.CloseWrite()
					}
				} else {
					_ = bridge.Close()
				}
				return
			}
			if data := chunk.GetData(); len(data) > 0 {
				if _, err := bridge.Write(data); err != nil {
					return
				}
			}
		}
	}()

	// bridge -> client. Remote EOF ends the RPC (full close, matching the
	// semantics of the old socat chain); queued Sends flush before trailers.
	buf := make([]byte, forwardChunkSize)
	for {
		n, rerr := bridge.Read(buf)
		if n > 0 {
			if serr := stream.Send(&fleetgrpc.ForwardChunk{Msg: &fleetgrpc.ForwardChunk_Data{Data: buf[:n]}}); serr != nil {
				return serr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			// The bridge died mid-stream (container stopped, forwarder
			// killed, or the recv pump closed it after the client vanished).
			return status.Errorf(codes.Unavailable, "bridge read: %v", rerr)
		}
	}
}

// --- stdio bridge (one subprocess per connection) ---------------------------

// stdioBridge adapts a running command's stdin/stdout to io.ReadWriteCloser.
// CloseWrite closes stdin (EOF for the in-container peer); reads return EOF
// when the process exits and its stdout drains.
type stdioBridge struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	closeOnce sync.Once
}

// startStdioBridge starts cmd with piped stdin/stdout and wraps them as the
// bridge. Stderr is dropped: per-connection bridge noise (e.g. socat's
// "connection refused") shows up to the client as an immediate close, same
// as the old spawned-socat chain.
func startStdioBridge(cmd *exec.Cmd) (io.ReadWriteCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &stdioBridge{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func (b *stdioBridge) Read(p []byte) (int, error)  { return b.stdout.Read(p) }
func (b *stdioBridge) Write(p []byte) (int, error) { return b.stdin.Write(p) }
func (b *stdioBridge) CloseWrite() error           { return b.stdin.Close() }

func (b *stdioBridge) Close() error {
	b.closeOnce.Do(func() {
		_ = b.stdin.Close()
		_ = b.stdout.Close()
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
		// Reap off the closing goroutine; Wait also releases the pipes.
		go func() { _ = b.cmd.Wait() }()
	})
	return nil
}

// --- spawned-forwarder bridge (coder / codespaces) ---------------------------

// forwarderBridge is a TCP connection to a backend forwarder process bound to
// a host-local ephemeral port; Close also kills the process so one forwarder
// lives exactly as long as its one connection. The reaper goroutine started
// in dialViaForwarder collects the exit status.
type forwarderBridge struct {
	net.Conn
	cmd       *exec.Cmd
	closeOnce sync.Once
}

func (b *forwarderBridge) CloseWrite() error {
	if tcp, ok := b.Conn.(*net.TCPConn); ok {
		return tcp.CloseWrite()
	}
	return nil
}

func (b *forwarderBridge) Close() error {
	b.closeOnce.Do(func() {
		_ = b.Conn.Close()
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
	})
	return nil
}

// dialViaForwarder starts the backend's listening forwarder on a host-local
// ephemeral port and dials it, polling until the CLI finishes its handshake
// and accepts.
func dialViaForwarder(b backend.Backend, containerID string, remotePort int) (io.ReadWriteCloser, error) {
	port, err := freeLocalPort()
	if err != nil {
		return nil, fmt.Errorf("pick local port: %w", err)
	}
	cmd := b.PortForwardCommand(containerID, port, remotePort)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start forwarder: %w", err)
	}
	// Sole Wait for this process: reaps it whether it dies during the dial
	// poll below or is killed later by forwarderBridge.Close.
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
	deadline := time.Now().Add(forwarderDialTimeout)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
		if dialErr == nil {
			return &forwarderBridge{Conn: conn, cmd: cmd}, nil
		}
		select {
		case <-exited:
			// The forwarder died (bad workspace, auth failure) — it will
			// never accept, so surface that instead of polling out the
			// deadline.
			return nil, fmt.Errorf("forwarder for %s:%d exited before accepting: %w", containerID, remotePort, dialErr)
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("forwarder on %s not accepting: %w", addr, dialErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// freeLocalPort asks the OS for an ephemeral port and releases it. The tiny
// claim-to-bind race is acceptable: the forwarder rebinding fails fast and
// the client just retries the connection.
func freeLocalPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}
