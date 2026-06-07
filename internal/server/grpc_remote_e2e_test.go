package server

import (
	"bufio"
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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// grpc_remote_e2e_test.go is the full-stack validation for tunneling gRPC through
// the gateway: a REAL fleetd gRPC server (token-gated) is exposed through a REAL
// gateway by a REAL remote.Manager tunnel, and driven by a REAL gRPC client that
// connects via the gateway's /grpc/<id> hijack endpoint. It exercises unary,
// server-streaming, and a BIDI round-trip — the last proves native HTTP/2 (incl.
// interleaved send/recv) survives the raw splice, the key transport guarantee.

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

// readerConn reads through a bufio.Reader (so handshake-buffered bytes aren't
// lost) but writes straight to the conn.
type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// testBearer attaches the token like the production gateway dialer's creds.
type testBearer struct{ token string }

func (b testBearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}
func (b testBearer) RequireTransportSecurity() bool { return false }

// grpcStack stands up the full real stack and returns a function that dials a
// fresh gRPC ClientConn through the gateway (optionally with the token), plus the
// token.
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

	// Real gateway on ephemeral TLS listeners.
	pool, certPath, keyPath := genTestTLSFiles(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	controlLn, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	t.Cleanup(func() { _ = controlLn.Close() })
	publicLn, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	t.Cleanup(func() { _ = publicLn.Close() })
	publicBase := "https://" + publicLn.Addr().String()

	gw, err := gateway.New(gateway.Config{PublicURL: publicBase, TLSCert: certPath, TLSKey: keyPath})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	go func() { _ = gw.ServeListeners(ctx, controlLn, publicLn) }()

	// Real manager with the gRPC listener wired.
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	controlAddr := controlLn.Addr().String()
	mgr := remote.NewManager(mcpPort, "e2e",
		func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		remote.WithDialFunc(func(dctx context.Context, _ string) (net.Conn, error) {
			return (&tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}).DialContext(dctx, "tcp", controlAddr)
		}),
		remote.WithGRPCListener(grpcLis),
	)
	go mgr.Run(ctx)
	mgr.Reconcile(true, "https://gw.example.com")

	// Wait for CONNECTED and derive the gRPC tunnel path from the MCP public URL.
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
	grpcPath := "/grpc/" + id
	publicAddr := publicLn.Addr().String()

	dial = func(withToken bool) *grpc.ClientConn {
		opts := []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(dctx context.Context, _ string) (net.Conn, error) {
				c, err := (&tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}).DialContext(dctx, "tcp", publicAddr)
				if err != nil {
					return nil, err
				}
				if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: gw\r\n\r\n", grpcPath); err != nil {
					_ = c.Close()
					return nil, err
				}
				br := bufio.NewReader(c)
				st, err := br.ReadString('\n')
				if err != nil || !strings.Contains(st, " 200 ") {
					_ = c.Close()
					return nil, fmt.Errorf("gateway handshake: %q err=%v", st, err)
				}
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						_ = c.Close()
						return nil, err
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				return readerConn{Conn: c, r: br}, nil
			}),
		}
		if withToken {
			opts = append(opts, grpc.WithPerRPCCredentials(testBearer{token: token}))
		}
		conn, err := grpc.NewClient("passthrough:///fleet-gateway", opts...)
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
		// would deadlock a blocking splice.
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
// unauthenticated RPC even though it reached fleetd through the tunnel.
func TestRemoteGRPCRejectsNoToken(t *testing.T) {
	dial, _ := grpcStack(t)
	client := fleetgrpc.NewFleetServiceClient(dial(false)) // no token creds

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Hello(ctx, &fleetgrpc.HelloRequest{ClientVersion: "e2e"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Hello without token through the tunnel: want Unauthenticated, got %v", err)
	}
}

// mkFleetDir ensures ~/.fleet exists (startMCPServer writes the token there).
func mkFleetDir() error { _, err := fleetpaths.EnsureDir(); return err }
