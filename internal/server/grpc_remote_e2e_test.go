package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// grpc_remote_e2e_test.go is the full-stack validation for tunneling gRPC through
// the gateway: a REAL fleetd gRPC server (token-gated) is exposed through a REAL
// gateway by a REAL remote.Manager tunnel, and driven by a REAL gRPC client that
// connects to the gateway's dedicated NATIVE-gRPC (h2c/h2) listener and routes by
// the fleet-session metadata header. It exercises unary, server-streaming, and a
// BIDI round-trip — the last proves native HTTP/2 (incl. interleaved send/recv and
// gRPC trailers) survives the gateway's h2c reverse proxy down the tunnel.

// --- a bidi echo service (uses real proto messages, so the default codec works) ---

const echoMethod = "/fleet.test.Echo/Bidi"

var echoDesc = grpc.ServiceDesc{
	ServiceName: "fleet.test.Echo",
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName:    "Bidi",
		ServerStreams: true,
		ClientStreams: true,
		Handler: func(_ any, stream grpc.ServerStream) error {
			for {
				var in fleetgrpc.HelloRequest
				if err := stream.RecvMsg(&in); err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}
				if err := stream.SendMsg(&fleetgrpc.HelloReply{ServerVersion: in.GetClientVersion()}); err != nil {
					return err
				}
			}
		},
	}},
}

// gatewayCreds attaches the fleet-session routing id (read by the gateway) and,
// when token is non-empty, the bearer token (validated by the daemon) — exactly
// like the production fleetclient gateway dialer.
type gatewayCreds struct{ id, token string }

func (c gatewayCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	md := map[string]string{"fleet-session": c.id}
	if c.token != "" {
		md["authorization"] = "Bearer " + c.token
	}
	return md, nil
}
func (c gatewayCreds) RequireTransportSecurity() bool { return false }

// grpcStack stands up the full real stack and returns a function that dials a
// fresh gRPC ClientConn through the gateway's gRPC listener (optionally with the
// token), plus the token.
func grpcStack(t *testing.T) (dial func(withToken bool) *grpc.ClientConn, token string) {
	t.Helper()
	isolateFleetDir(t)
	if err := mkFleetDir(); err != nil {
		t.Fatalf("mkdir fleet dir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	svc := newService()
	go svc.hub.run(ctx)

	mcpHTTP, mcpPort := startMCPServer(svc)
	if mcpHTTP == nil {
		t.Fatal("MCP server failed to start")
	}
	t.Cleanup(func() { _ = mcpHTTP.Close() })
	var err error
	token, err = loadOrCreateMCPToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	// Tunnel-facing gRPC server (FleetService + echo), token-gated, fed by the demux.
	grpcLis := remote.NewChanListener()
	authUnary, authStream := bearerAuthInterceptors(token)
	tunnelGRPC := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(tunnelGRPC, svc)
	tunnelGRPC.RegisterService(&echoDesc, nil)
	go func() { _ = tunnelGRPC.Serve(grpcLis) }()
	t.Cleanup(tunnelGRPC.Stop)

	// Real gateway on ephemeral TLS listeners. The gRPC listener is a PLAIN listener
	// (the gateway adds TLS via ServeTLS so HTTP/2 is negotiated over ALPN).
	pool, certPath, keyPath := genTestTLSFiles(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	publicLn, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	t.Cleanup(func() { _ = publicLn.Close() })
	// The gRPC listener (plain — grpc.Creds adds TLS) hosts BOTH the remote-control
	// gRPC proxy AND fleetd registration (the Register bidi stream).
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	t.Cleanup(func() { _ = grpcLn.Close() })
	publicBase := "https://" + publicLn.Addr().String()

	gw, err := gateway.New(gateway.Config{PublicURL: publicBase, TLSCert: certPath, TLSKey: keyPath})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	go func() { _ = gw.ServeListeners(ctx, publicLn, grpcLn) }()

	// Real manager registering over the gateway's gRPC endpoint, with the gRPC
	// listener wired so the tunnel demuxes TagGRPC streams.
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	gwCreds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	mgr := remote.NewManager(mcpPort, "e2e",
		func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		remote.WithDialFunc(registerDialFunc(grpcLn.Addr().String(), gwCreds)),
		remote.WithGRPCListener(grpcLis),
	)
	go mgr.Run(ctx)
	// Both toggles on: remote MCP (the tunnel's base traffic) AND remote fleet
	// (without which the grpc feature is no longer advertised, and every
	// remote-control RPC under test here would be refused).
	mgr.Reconcile(true, true, "https://gw.example.com")

	// Wait for CONNECTED and derive the session id from the MCP public URL.
	var publicURL string
	deadline := time.After(10 * time.Second)
	for publicURL == "" {
		select {
		case st := <-statusCh:
			if st.GetState() == fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				publicURL = st.GetPublicUrl()
			}
		case <-deadline:
			t.Fatal("tunnel never connected")
		}
	}
	u, _ := url.Parse(publicURL)
	id := strings.TrimPrefix(u.Path, "/mcp/")
	grpcAddr := grpcLn.Addr().String()

	dial = func(withToken bool) *grpc.ClientConn {
		creds := gatewayCreds{id: id}
		if withToken {
			creds.token = token
		}
		conn, err := grpc.NewClient("dns:///"+grpcAddr,
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})),
			grpc.WithPerRPCCredentials(creds),
		)
		if err != nil {
			t.Fatalf("grpc client: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	return dial, token
}

func TestRemoteGRPCEndToEnd(t *testing.T) {
	dial, _ := grpcStack(t)
	conn := dial(true)
	client := fleetgrpc.NewFleetServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Unary through the tunnel ---
	hello, err := client.Hello(ctx, &fleetgrpc.HelloRequest{ClientVersion: "e2e"})
	if err != nil {
		t.Fatalf("Hello through gateway gRPC: %v", err)
	}
	if hello.GetPid() == 0 {
		t.Fatal("Hello returned an empty reply")
	}

	// --- Server-streaming through the tunnel (Watch initial snapshot) ---
	ws, err := client.Watch(ctx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatalf("Watch open: %v", err)
	}
	ev, err := ws.Recv()
	if err != nil {
		t.Fatalf("Watch recv: %v", err)
	}
	if ev.GetStateChanged() == nil {
		t.Fatalf("first Watch event is not StateChanged: %T", ev.GetKind())
	}

	// --- Bidi round-trip through the tunnel (interleaved send/recv) ---
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Bidi", ServerStreams: true, ClientStreams: true}, echoMethod)
	if err != nil {
		t.Fatalf("open bidi: %v", err)
	}
	for i := 0; i < 4; i++ {
		msg := fmt.Sprintf("ping-%d", i)
		if err := stream.SendMsg(&fleetgrpc.HelloRequest{ClientVersion: msg}); err != nil {
			t.Fatalf("bidi send %d: %v", i, err)
		}
		// Read the echo BEFORE sending the next — the interleaving pattern that
		// would deadlock a half-duplex proxy.
		var got fleetgrpc.HelloReply
		if err := stream.RecvMsg(&got); err != nil {
			t.Fatalf("bidi recv %d: %v", i, err)
		}
		if got.GetServerVersion() != msg {
			t.Fatalf("bidi echo %d = %q, want %q", i, got.GetServerVersion(), msg)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	var trailing fleetgrpc.HelloReply
	if err := stream.RecvMsg(&trailing); err != io.EOF {
		t.Fatalf("after CloseSend want io.EOF, got %v", err)
	}
}

// TestRemoteGRPCRejectsNoToken confirms the daemon's interceptor rejects an
// unauthenticated RPC even though it reached fleetd through the tunnel (the
// fleet-session header only routes; it is not a credential).
func TestRemoteGRPCRejectsNoToken(t *testing.T) {
	dial, _ := grpcStack(t)
	client := fleetgrpc.NewFleetServiceClient(dial(false)) // routing id only, no token

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Hello(ctx, &fleetgrpc.HelloRequest{ClientVersion: "e2e"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Hello without token through the tunnel: want Unauthenticated, got %v", err)
	}
}

// mkFleetDir ensures ~/.fleet exists (startMCPServer writes the token there).
func mkFleetDir() error { _, err := fleetpaths.EnsureDir(); return err }
