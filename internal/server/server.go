// Package server is the fleet daemon (fleetd): the single, long-running,
// per-user process that owns ~/.fleet and (in later phases) all backend
// operations. CLI and TUI clients talk to it over gRPC on a unix socket.
//
// This package is SERVER-ONLY. Client code (internal/cli, internal/tui,
// internal/fleetclient) must never import it — except internal/cli/server.go,
// which is the entrypoint that runs the daemon.
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc"
)

// Serve runs the fleet server until ctx is cancelled (signal) or a Shutdown RPC
// arrives. It returns nil when another server already holds the lifetime lock
// (this invocation is a redundant spawn), so racing spawns exit cleanly.
func Serve(ctx context.Context) error {
	dir := fleetpaths.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create fleet dir: %w", err)
	}

	// Lifetime lock: exactly one fleet server per user. If another server holds
	// it, we're a redundant spawn (a client double-spawned, or a stale socket
	// fooled someone) — exit cleanly so the caller uses the existing server.
	lockFD, err := acquireServerLock()
	if err != nil {
		flog.Info("fleet server already running; redundant spawn exiting", "pid", os.Getpid())
		return nil
	}
	defer releaseLock(lockFD)

	// Holding the lifetime lock proves no other server exists, so any leftover
	// socket file is stale and safe to remove (net.Listen fails if it exists).
	sock := fleetpaths.SocketPath()
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	lis, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sock, err)
	}
	// 0600: host-local, per-user. Unlike the bind-mounted control socket,
	// nothing cross-UID connects here.
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = lis.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	svc := newService()

	// Start the hub (the broadcast-model owner) and the state poller (the
	// non-authoritative source of truth that re-Loads state.json) on a context
	// cancelled at shutdown. Cancelling stops hub.run, which closes hub.done so
	// in-flight Watch pumps return — otherwise GracefulStop would block on a
	// long-lived Watch stream.
	hubCtx, cancelHub := context.WithCancel(ctx)
	defer cancelHub()
	// Background work spawned by RPC handlers (e.g. the TUI-connect buildkit
	// reconcile) derives from hubCtx so it stops on shutdown. Set before Serve
	// starts accepting RPCs, so no handler observes the zero value.
	svc.bgCtx = hubCtx
	go svc.hub.run(hubCtx)
	go runStatePoller(hubCtx, svc.hub)
	// Runtime pollers (live status / stats+activity / sessions). Gated on a
	// runtime subscriber, so they stay idle until a TUI connects with
	// subscribe_runtime — no backend traffic for plain `fleet ls`.
	startRuntimePollers(hubCtx, svc.hub)

	// Control-socket listeners: the server owns every running instance's control
	// socket and turns received browser.open envelopes (from an in-container
	// `fleet launch` TUI) into BrowserOpen events the connected client execs
	// locally, and file.copy envelopes (from an in-container `fleet copy` / fc)
	// into FileCopy events the connected client performs the scp-style copy for.
	// Runs unconditionally (cheap: only running instances get a listener) and
	// tears down when hubCtx is cancelled.
	controlReg := newControlRegistry(func(fleetName, instanceName, url string) {
		svc.hub.post(func(h *hub) {
			h.broadcastBrowserOpen(&fleetgrpc.BrowserOpen{
				Url:      url,
				Fleet:    fleetName,
				Instance: instanceName,
			})
		})
	}, func(fleetName, instanceName, src, dst string) {
		svc.hub.post(func(h *hub) {
			h.broadcastFileCopy(&fleetgrpc.FileCopy{
				Fleet:    fleetName,
				Instance: instanceName,
				Src:      src,
				Dst:      dst,
			})
		})
	}, svc.hub.hasSubscribers)
	go controlReg.run(hubCtx)

	grpcServer := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(grpcServer, svc)

	// MCP HTTP server: a second listener exposing the non-interactive CLI subset
	// as MCP tools (see mcp.go). Auxiliary to gRPC — startMCPServer returns
	// (nil, 0) if it can't bind, in which case the daemon runs without MCP. The
	// deferred Shutdown drains it and removes the port file on every return path,
	// mirroring the version-file handling below.
	mcpHTTP, mcpPort := startMCPServer(svc)
	if mcpHTTP != nil {
		defer func() {
			// Bounded drain: an MCP tool wedged on a stuck backend (e.g. a hung
			// docker exec) must not pin daemon teardown. Give in-flight requests a
			// few seconds, then force-close so Serve always returns. Tool handlers
			// also watch hubCtx (cancelled above), so they normally unblock first.
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := mcpHTTP.Shutdown(sctx); err != nil {
				_ = mcpHTTP.Close()
			}
			// Remove only the liveness hint. The token and env snippet persist
			// across restarts so configured MCP clients (mcp.json) keep working.
			_ = os.Remove(fleetpaths.McpPortPath())
		}()
	}

	// Tunnel-facing gRPC server: exposes the SAME FleetService as the local unix
	// socket, but gated by the MCP bearer token (the local socket stays auth-less).
	// It is Served over an in-memory listener fed by the tunnel demux, so there is
	// no extra port/socket; the gateway tunnels gRPC alongside MCP whenever both
	// ends negotiate FeatureGRPC. Only wired when MCP is up (we have a port +
	// token); enabling remote MCP in config exposes this too (one toggle).
	var remoteOpts []remote.Option
	if mcpPort != 0 {
		if token, err := loadOrCreateMCPToken(); err == nil {
			grpcLis := remote.NewChanListener()
			authUnary, authStream := bearerAuthInterceptors(token)
			tunnelGRPC := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
			fleetgrpc.RegisterFleetServiceServer(tunnelGRPC, svc)
			go func() { _ = tunnelGRPC.Serve(grpcLis) }()
			defer tunnelGRPC.Stop()
			remoteOpts = append(remoteOpts, remote.WithGRPCListener(grpcLis))
		} else {
			flog.Warn("remote grpc: load token", "err", err)
		}
	}

	// Remote gateway tunnel: an outbound, OPT-IN connection that exposes the
	// loopback MCP server ("Enable Remote MCP") and/or the gRPC server above
	// ("Enable Remote Fleet") to the internet through a remote fleet gateway.
	// Like MCP itself it is auxiliary — the supervisor stays idle until the
	// config enables a traffic kind, and a connect failure only affects remote
	// access, never the local daemon. It publishes its status (incl. the
	// gateway-assigned Public MCP URL / Public GRPC URL) through the hub so the
	// TUI settings page reflects it live.
	svc.remote = remote.NewManager(mcpPort, versionOrDev(), func(st *fleetgrpc.RemoteMcpStatus) {
		svc.hub.post(func(h *hub) { h.broadcastRemoteMcpStatus(st) })
	}, remoteOpts...)
	go svc.remote.Run(hubCtx)
	if cfg, err := state.LoadConfig(); err == nil {
		svc.remote.Reconcile(cfg.RemoteMcpSettings.Enabled, cfg.RemoteMcpSettings.FleetEnabled, cfg.RemoteMcpSettings.GatewayURL)
	} else {
		flog.Warn("remote mcp: load config", "err", err)
	}

	writeVersionFile()
	defer func() { _ = os.Remove(fleetpaths.VersionFilePath()) }()

	flog.Info("fleet server started", "pid", os.Getpid(), "socket", sock, "version", versionOrDev(), "mcpPort", mcpPort)

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()

	select {
	case <-ctx.Done():
		flog.Info("fleet server stopping (signal)", "pid", os.Getpid())
		cancelHub()
		grpcServer.GracefulStop()
	case <-svc.shutdownCh:
		flog.Info("fleet server stopping (Shutdown RPC)", "pid", os.Getpid())
		cancelHub()
		grpcServer.GracefulStop()
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}
	return nil
}

// acquireServerLock takes the exclusive flock held for the server's whole
// lifetime. The returned *os.File must stay open to hold the lock.
//
// It retries briefly rather than failing on first contention: during a restart
// the outgoing server releases this lock a moment after it stops serving, and
// the incoming server (spawned once the old one stopped answering) should win it
// rather than exit as "redundant". A genuinely-redundant spawn (another healthy
// server holding the lock the whole window) simply waits it out, then exits.
func acquireServerLock() (*os.File, error) {
	f, err := os.OpenFile(fleetpaths.ServerLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, lockErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// writeVersionFile records the running version as a cheap pre-dial hint. It is
// never authoritative — clients verify via the Hello handshake.
func writeVersionFile() {
	_ = os.WriteFile(fleetpaths.VersionFilePath(), []byte(versionOrDev()), 0o600)
}

func versionOrDev() string {
	if version.Version == "" {
		return "dev"
	}
	return version.Version
}
