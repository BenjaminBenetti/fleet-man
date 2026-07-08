package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
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
	// The token gates access: the TCP port is reachable by any local user, so
	// (unlike the 0600 unix socket) it is not itself a boundary. Stored 0600 so
	// only the owning user can read it, and reused across restarts so env vars /
	// mcp.json stay valid. If we can't establish it, disable MCP rather than
	// expose unauthenticated fleet-control tools across the user boundary.
	token, err := loadOrCreateMCPToken()
	if err != nil {
		flog.Error("mcp token", "err", err)
		return nil, 0
	}

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
	httpServer := &http.Server{Handler: mcpAuth(token, handler)}

	// Record the port AFTER the bind so the file always names the real port.
	// Best-effort (like writeVersionFile): a write failure just means clients
	// fall back to probing from mcpDefaultPort. 0o600 matches the other
	// per-user host-local discovery files in ~/.fleet.
	if err := atomicfile.Write(fleetpaths.McpPortPath(), []byte(strconv.Itoa(port)), 0o600); err != nil {
		flog.Warn("write mcp.port", "err", err)
	}

	// Publish the endpoint as a sourceable env snippet and wire ~/.bashrc to it,
	// so MCP client configs (mcp.json) can reference ${FLEET_MCP_URL} /
	// ${FLEET_MCP_TOKEN} without copying the rotating port or the secret by hand.
	writeMCPEnv(port, token)
	ensureBashrcSourcesMCPEnv()

	go func() {
		if err := httpServer.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			flog.Error("mcp serve", "err", err)
		}
	}()
	flog.Info("mcp server started", "port", port)
	return httpServer, port
}

// newMCPToken returns a fresh high-entropy bearer token (256 bits, hex).
func newMCPToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loadOrCreateMCPToken reuses the persisted token if present (so env vars and
// mcp.json stay valid across restarts), otherwise mints and persists a new one.
func loadOrCreateMCPToken() (string, error) {
	if data, err := os.ReadFile(fleetpaths.McpTokenPath()); err == nil {
		if t := strings.TrimSpace(string(data)); t != "" {
			return t, nil
		}
	}
	token, err := newMCPToken()
	if err != nil {
		return "", err
	}
	if err := atomicfile.Write(fleetpaths.McpTokenPath(), []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// mcpBashrcMarker is grep'd against ~/.bashrc to detect a prior wire-in, so the
// source block is appended at most once (mirrors internal/fleetlaunch).
const mcpBashrcMarker = ".fleet/mcp.env"

// writeMCPEnv refreshes ~/.fleet/mcp.env with the live endpoint as exports.
// 0600 because it embeds the token. Best-effort: a failure just means clients
// fall back to reading mcp.port / mcp.token directly.
func writeMCPEnv(port int, token string) {
	content := fmt.Sprintf(`# Written by fleet-man on MCP server startup; sourced from ~/.bashrc.
# MCP clients (mcp.json) can use ${FLEET_MCP_URL} and Authorization: Bearer ${FLEET_MCP_TOKEN}.
export FLEET_MCP_PORT=%d
export FLEET_MCP_URL=http://127.0.0.1:%d
export FLEET_MCP_TOKEN=%s
`, port, port, token)
	if err := atomicfile.Write(fleetpaths.McpEnvPath(), []byte(content), 0o600); err != nil {
		flog.Warn("write mcp.env", "err", err)
	}
}

// ensureBashrcSourcesMCPEnv appends a marker-guarded block to ~/.bashrc that
// sources mcp.env when present, so new shells (and the MCP clients launched from
// them) pick up FLEET_MCP_* automatically. Idempotent and best-effort; mirrors
// the in-container wire-in in internal/fleetlaunch.
func ensureBashrcSourcesMCPEnv() {
	home := os.Getenv("HOME")
	if home == "" {
		return
	}
	bashrc := filepath.Join(home, ".bashrc")
	if data, err := os.ReadFile(bashrc); err == nil && strings.Contains(string(data), mcpBashrcMarker) {
		return // already wired (or hand-edited in)
	}
	block := "\n# Added by fleet-man — export the MCP endpoint env when the server is up.\n" +
		`[ -f "$HOME/.fleet/mcp.env" ] && . "$HOME/.fleet/mcp.env"` + "\n"
	f, err := os.OpenFile(bashrc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		flog.Warn("wire mcp.env into .bashrc", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		flog.Warn("wire mcp.env into .bashrc", "err", err)
	}
}

// mcpAuth requires every MCP request to carry "Authorization: Bearer <token>"
// (constant-time compared). This is the access boundary for the loopback TCP
// endpoint: only a process that can read ~/.fleet/mcp.token (0600, so same-user
// only) knows the token, matching the unix socket's per-user protection.
func mcpAuth(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
// call the streaming service handlers (Logs, ...) directly and collect their
// emitted events synchronously. The embedded grpc.ServerStream is nil — the
// handlers only ever call Send and Context, so the other ServerStream methods
// (SetHeader/RecvMsg/...) are never invoked.
type streamCollector[T any] struct {
	grpc.ServerStream
	ctx    context.Context
	events []*T
}

func (c *streamCollector[T]) Send(ev *T) error         { c.events = append(c.events, ev); return nil }
func (c *streamCollector[T]) Context() context.Context { return c.ctx }

// awaitJob blocks until j emits its JobDone (or ctx is cancelled) and returns
// the job's single result: the final instance record, any non-fatal warnings,
// and an error if the job failed. The job runs in a server-owned goroutine
// decoupled from ctx — cancellation abandons the WAIT, never the job (matching
// the gRPC path, where a dropped stream leaves the job running).
func awaitJob(ctx context.Context, j *job) (final *fleetgrpc.Instance, warnings []string, err error) {
	hist, ch := j.subscribe()
	for _, ev := range hist {
		if d := ev.GetDone(); d != nil {
			return doneOutcome(d)
		}
	}
	if ch == nil {
		// A terminal history without a JobDone violates the jobs.proto contract;
		// surface it rather than hanging.
		return nil, nil, errors.New("job ended without a result")
	}
	defer j.unsubscribe(ch)
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil, nil, errors.New("job ended without a result")
			}
			if d := ev.GetDone(); d != nil {
				return doneOutcome(d)
			}
		}
	}
}

// doneOutcome unpacks a JobDone into awaitJob's result shape: a failed job
// becomes an error carrying the job's message.
func doneOutcome(d *fleetgrpc.JobDone) (*fleetgrpc.Instance, []string, error) {
	if !d.GetSuccess() {
		return d.GetInstance(), d.GetWarnings(), errors.New(d.GetError())
	}
	return d.GetInstance(), d.GetWarnings(), nil
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
