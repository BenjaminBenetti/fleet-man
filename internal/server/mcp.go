package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
)

// mcp.go runs the MCP (Model Context Protocol) HTTP server alongside the gRPC
// daemon. It exposes the non-interactive subset of the fleet CLI — list/status/
// version/logs, the create/start/stop/down/destroy/clone lifecycle, one-shot
// exec, and tmux session read/write — as MCP tools over Streamable HTTP so AI
// agents (and any MCP client) can drive fleet programmatically. Interactive,
// open-ended streams (`fleet shell`, `fleet logs --follow`) are deliberately
// omitted: they don't translate to a single tool call/result.
//
// The MCP server lives IN this package (not a subpackage) because every tool
// reuses the unexported *service methods (GetState, CreateInstance, Logs, ...)
// and server-internal helpers (resolveServerInstance) directly — the same
// in-process path the gRPC handlers take, with no socket hop.

// mcpDefaultPort is the first port the MCP HTTP server tries. If it is taken,
// listenMCP increments until it finds a free one; the chosen port is written to
// ~/.fleet/mcp.port for discovery.
const mcpDefaultPort = 6012

// mcpMaxPortProbes bounds the port search so a pathological "everything bound"
// host can't spin forever.
const mcpMaxPortProbes = 100

// mcpSessionTimeout reaps idle MCP sessions. The Streamable HTTP transport is
// stateful: each client `initialize` creates a server session (and a goroutine)
// that a dropped connection does NOT close — only a client DELETE, this timeout,
// or process exit does. Since fleetd is long-lived, an unbounded timeout would
// leak a session per crashed/abandoned client, so we expire idle ones.
const mcpSessionTimeout = 30 * time.Minute

// startMCPServer binds the MCP HTTP server to the first free loopback port at or
// above mcpDefaultPort, records it in ~/.fleet/mcp.port, and serves in a
// background goroutine. It returns the *http.Server (so the caller can drain it
// on shutdown) and the bound port, or (nil, 0) if no port could be bound.
//
// MCP is AUXILIARY to the gRPC transport: a bind failure logs and disables MCP
// rather than aborting the daemon, so a port-exhausted host still gets a working
// `fleet` CLI/TUI.
func startMCPServer(svc *service) (*http.Server, int) {
	lis, port, err := listenMCP(mcpDefaultPort)
	if err != nil {
		flog.Error("mcp listen", "err", err)
		return nil, 0
	}

	mcpSrv := newMCPServer(svc)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpSrv },
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout},
	)
	httpServer := &http.Server{Handler: handler}

	// Record the port AFTER the bind so the file always names the real port.
	// Best-effort (like writeVersionFile): a write failure just means clients
	// fall back to probing from mcpDefaultPort. 0o600 matches the other
	// per-user host-local discovery files in ~/.fleet.
	if err := os.WriteFile(fleetpaths.McpPortPath(), []byte(strconv.Itoa(port)), 0o600); err != nil {
		flog.Warn("write mcp.port", "err", err)
	}

	go func() {
		if err := httpServer.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			flog.Error("mcp serve", "err", err)
		}
	}()
	flog.Info("mcp server started", "port", port)
	return httpServer, port
}

// listenMCP binds the MCP HTTP server to the first free 127.0.0.1 port at or
// above startPort. It returns the HELD listener (hand it straight to
// http.Server.Serve, so there is no close/re-bind race) and the chosen port.
// Loopback-only mirrors the unix socket's host-local, per-user scope — the MCP
// endpoint is for local agents, not the network.
func listenMCP(startPort int) (net.Listener, int, error) {
	for port := startPort; port < startPort+mcpMaxPortProbes; port++ {
		lis, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return lis, port, nil
		}
		// Only "port already in use" means "try the next one"; any other error
		// (e.g. permission, no loopback) is fatal, mirroring how a failed
		// net.Listen aborts the rest of Serve.
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
		}
	}
	return nil, 0, fmt.Errorf("no free port in [%d,%d)", startPort, startPort+mcpMaxPortProbes)
}

// newMCPServer builds the MCP server and registers every fleet tool. Long-running
// lifecycle tools scope their job relay to svc.bgCtx (the daemon's shutdown
// context) so they unblock promptly when the daemon stops.
func newMCPServer(svc *service) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fleet", Version: versionOrDev()}, nil)
	registerMCPTools(srv, svc)
	return srv
}

// streamCollector is an in-process gRPC server stream that captures every Send
// into a slice instead of writing to a real transport. It lets the MCP tools
// call the streaming service handlers (CreateInstance, Logs, ...) directly and
// collect their emitted events synchronously. The embedded grpc.ServerStream is
// nil — the handlers only ever call Send and Context, so the other
// ServerStream methods (SetHeader/RecvMsg/...) are never invoked.
type streamCollector[T any] struct {
	grpc.ServerStream
	ctx    context.Context
	events []*T
}

func (c *streamCollector[T]) Send(ev *T) error         { c.events = append(c.events, ev); return nil }
func (c *streamCollector[T]) Context() context.Context { return c.ctx }

// runLifecycleJob drives one server-streaming lifecycle RPC (CreateInstance,
// StartInstance, ...) to completion in-process and returns its single result:
// the final instance record, any non-fatal warnings, and an error if the job
// failed or never produced a JobDone.
//
// The lifecycle handlers block until the job emits JobDone (relay forwards
// history+live events until the terminal one), so this call is synchronous with
// respect to job completion. The job itself runs in a server-owned goroutine and
// is decoupled from ctx: if ctx is cancelled the handler returns early but the
// job keeps running server-side (matching the gRPC path).
func runLifecycleJob(ctx context.Context, open func(grpc.ServerStreamingServer[fleetgrpc.JobEvent]) error) (final *fleetgrpc.Instance, warnings []string, err error) {
	c := &streamCollector[fleetgrpc.JobEvent]{ctx: ctx}
	// A non-nil error here is a pre-job gRPC status (InvalidArgument/NotFound/
	// AlreadyExists) returned before any JobDone was produced.
	if rpcErr := open(c); rpcErr != nil {
		return nil, nil, rpcErr
	}
	for _, ev := range c.events {
		if d := ev.GetDone(); d != nil {
			if !d.GetSuccess() {
				return d.GetInstance(), d.GetWarnings(), errors.New(d.GetError())
			}
			return d.GetInstance(), d.GetWarnings(), nil
		}
	}
	return nil, nil, errors.New("job ended without a result")
}

// mergeCtx returns a context cancelled when EITHER input is cancelled, plus a
// cancel to release the watcher. Tools use it so work unblocks on daemon
// shutdown (the primary, daemon-scoped bgCtx) OR when the MCP session closes
// (the handler ctx). For lifecycle tools the job itself keeps running
// server-side regardless — this only governs how long the tool waits on it.
func mergeCtx(primary, secondary context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(primary)
	stop := context.AfterFunc(secondary, cancel)
	return ctx, func() { stop(); cancel() }
}

// protoBackend maps a concrete backend type to its proto enum. An empty/unknown
// type becomes UNSPECIFIED, which the server resolves to the configured default.
func protoBackend(b fleet.BackendType) fleetgrpc.BackendType {
	switch b {
	case fleet.BackendCoder:
		return fleetgrpc.BackendType_BACKEND_TYPE_CODER
	case fleet.BackendCodespaces:
		return fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES
	case fleet.BackendDevcontainer:
		return fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER
	default:
		return fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED
	}
}
