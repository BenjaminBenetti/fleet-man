package fleetclient

import (
	"context"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Conn is a connected fleet client. Service() returns the gRPC client; callers
// never see the socket, address, or transport.
type Conn struct {
	conn *grpc.ClientConn
	svc  fleetgrpc.FleetServiceClient
}

// Service returns the fleet service client for issuing RPCs.
func (c *Conn) Service() fleetgrpc.FleetServiceClient { return c.svc }

// Close releases the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Dial connects to the fleet server and returns a ready Conn. For a local
// endpoint it auto-spawns the server if it isn't running and runs the Hello
// version handshake (relaunching a too-old local server, or erroring on an
// incompatible remote/newer one). For a remote endpoint it dials only.
func Dial(ctx context.Context) (*Conn, error) {
	ep := selectEndpoint()
	conn, err := grpc.NewClient(ep.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", ep, err)
	}

	svc := fleetgrpc.NewFleetServiceClient(conn)

	reply, err := hello(ctx, svc)
	spawned := false
	if err != nil {
		if !ep.IsLocal() {
			_ = conn.Close()
			return nil, fmt.Errorf("fleet server not reachable at %s: %w", ep, err)
		}
		if spawnErr := ensureServerLocal(ctx, ep); spawnErr != nil {
			_ = conn.Close()
			return nil, spawnErr
		}
		spawned = true
		// The first Hello put this lazy conn into TRANSIENT_FAILURE (the socket
		// didn't exist yet) with connection backoff. Now that the server is up,
		// kick it out of backoff immediately and retry with WaitForReady so the
		// RPC waits for the (re)connect instead of failing fast on the stale error.
		conn.Connect()
		reply, err = helloReady(ctx, svc)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("fleet server unreachable after spawn: %w", err)
		}
	}

	restarted, err := reconcileServer(ctx, ep, svc, reply, spawned)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if restarted {
		// The server we were talking to was replaced; settle the long-lived conn
		// onto the new process (same socket path) before handing it back.
		conn.Connect()
		if _, err := helloReady(ctx, svc); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("fleet server unreachable after restart: %w", err)
		}
	}
	return &Conn{conn: conn, svc: svc}, nil
}

func hello(ctx context.Context, svc fleetgrpc.FleetServiceClient) (*fleetgrpc.HelloReply, error) {
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return svc.Hello(hctx, &fleetgrpc.HelloRequest{ClientVersion: version.Version})
}

// helloReady is hello with WaitForReady, used right after a spawn so the RPC
// waits for the connection to come up rather than failing fast while the
// channel is still in connection backoff from the pre-spawn attempt.
func helloReady(ctx context.Context, svc fleetgrpc.FleetServiceClient) (*fleetgrpc.HelloReply, error) {
	hctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	return svc.Hello(hctx, &fleetgrpc.HelloRequest{ClientVersion: version.Version}, grpc.WaitForReady(true))
}
