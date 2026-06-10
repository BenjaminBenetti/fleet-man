package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpc_route_test.go covers the dedicated native-gRPC listener at the gateway
// level: a real gRPC client reaches the gateway's gRPC port, the gateway routes by
// the fleet-session header, and transparently proxies to a gRPC server standing in
// for the daemon over a TagGRPC tunnel stream. It exercises BOTH transports — h2c
// (plaintext) and h2 (TLS) — and the routing rejections. (The full real-FleetService
// validation, incl. streaming/bidi/trailers, is in
// internal/server/grpc_remote_e2e_test.go.)

// streamListener is a net.Listener fed by Push, so a grpc.Server can Serve the
// demuxed TagGRPC tunnel streams.
type streamListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newStreamListener() *streamListener {
	return &streamListener{conns: make(chan net.Conn), done: make(chan struct{})}
}
func (l *streamListener) push(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}
func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *streamListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *streamListener) Addr() net.Addr { return tunnelAddr{} }

type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "tunnel" }
func (tunnelAddr) String() string  { return "tunnel" }

// rawEchoServer is a grpc.Server (raw codec) that echoes request frames — a
// stand-in for the daemon's grpc.Server.
func rawEchoServer() *grpc.Server {
	srv := grpc.NewServer(grpc.ForceServerCodec(tunnel.RawCodec{}), grpc.UnknownServiceHandler(
		func(_ any, ss grpc.ServerStream) error {
			f := &tunnel.RawFrame{}
			for {
				if err := ss.RecvMsg(f); err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}
				if err := ss.SendMsg(f); err != nil {
					return err
				}
			}
		}))
	return srv
}

// dialFleetdGRPC simulates a fleetd that negotiates gRPC over a TLS-verified
// Register stream: it serves rawEchoServer over each demuxed TagGRPC stream.
func dialFleetdGRPC(t *testing.T, grpcAddr string, pool *x509.CertPool) tunnel.RegisterReply {
	t.Helper()
	conn := openRegisterStream(t, grpcAddr, credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}))
	return registerFleetdGRPC(t, conn)
}

// dialFleetdGRPCPlain is dialFleetdGRPC over a plaintext (h2c) Register stream.
func dialFleetdGRPCPlain(t *testing.T, grpcAddr string) tunnel.RegisterReply {
	t.Helper()
	conn := openRegisterStream(t, grpcAddr, insecure.NewCredentials())
	return registerFleetdGRPC(t, conn)
}

// registerFleetdGRPC performs the gRPC-negotiating handshake and serves a raw-echo
// grpc.Server over each TagGRPC stream.
func registerFleetdGRPC(t *testing.T, conn net.Conn) tunnel.RegisterReply {
	t.Helper()
	if err := tunnel.WriteFrame(conn, tunnel.RegisterRequest{Features: []string{tunnel.FeatureGRPC}}); err != nil {
		t.Fatalf("write register: %v", err)
	}
	var reply tunnel.RegisterReply
	if err := tunnel.ReadFrame(conn, &reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply.Error != "" {
		t.Fatalf("gateway refused: %s", reply.Error)
	}
	sess, err := tunnel.ClientSession(conn, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}

	lis := newStreamListener()
	srv := rawEchoServer()
	go func() { _ = srv.Serve(lis) }()
	go func() {
		for {
			stream, err := sess.Accept()
			if err != nil {
				return
			}
			go func(stream net.Conn) {
				tag, err := tunnel.ReadTag(stream)
				if err != nil || tag != tunnel.TagGRPC {
					_ = stream.Close()
					return
				}
				lis.push(stream)
			}(stream)
		}
	}()

	t.Cleanup(func() { srv.Stop(); _ = lis.Close(); _ = sess.Close(); _ = conn.Close() })
	return reply
}

// grpcEcho dials the gateway's gRPC port and round-trips one raw frame for session
// id, returning the echoed payload and any RPC error.
func grpcEcho(t *testing.T, creds credentials.TransportCredentials, grpcAddr, id, payload string) (string, error) {
	t.Helper()
	conn, err := grpc.NewClient("dns:///"+grpcAddr,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(tunnel.RawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc client: %v", err)
	}
	defer conn.Close()

	ctx := context.Background()
	if id != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, grpcSessionHeader, id)
	}
	cs, err := conn.NewStream(ctx, proxyStreamDesc, "/fleet.test/Echo")
	if err != nil {
		return "", err
	}
	if err := cs.SendMsg(&tunnel.RawFrame{Payload: []byte(payload)}); err != nil {
		return "", err
	}
	if err := cs.CloseSend(); err != nil {
		return "", err
	}
	out := &tunnel.RawFrame{}
	if err := cs.RecvMsg(out); err != nil {
		return "", err
	}
	return string(out.Payload), nil
}

func tlsCreds(pool *x509.CertPool) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
}

// TestGatewayGRPCRoute drives the gRPC port over TLS (h2): a frame routed by the
// fleet-session header is proxied down the tunnel and echoed back.
func TestGatewayGRPCRoute(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, _, grpcAddr := startTestGateway(t, cert, "https://gw.example.com")

	reply := dialFleetdGRPC(t, grpcAddr, pool)
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		t.Fatalf("gateway did not negotiate grpc: features=%v", reply.Features)
	}
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	got, err := grpcEcho(t, tlsCreds(pool), grpcAddr, id, "hello")
	if err != nil || got != "hello" {
		t.Fatalf("grpc route over TLS = (%q, %v), want (hello, nil)", got, err)
	}
}

// TestGatewayGRPCRoutePlainHTTP drives the gRPC port over h2c (no TLS), the
// reverse-proxy / TLS-terminated-upstream path.
func TestGatewayGRPCRoutePlainHTTP(t *testing.T) {
	s, _, grpcAddr := startTestGatewayPlain(t, "http://gw.example.com")

	reply := dialFleetdGRPCPlain(t, grpcAddr)
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		t.Fatalf("gateway did not negotiate grpc: features=%v", reply.Features)
	}
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	got, err := grpcEcho(t, insecure.NewCredentials(), grpcAddr, id, "plain")
	if err != nil || got != "plain" {
		t.Fatalf("grpc route over h2c = (%q, %v), want (plain, nil)", got, err)
	}
}

// TestGatewayGRPCMissingSession confirms a request with no fleet-session metadata
// is rejected with InvalidArgument.
func TestGatewayGRPCMissingSession(t *testing.T) {
	_, _, grpcAddr := startTestGatewayPlain(t, "http://gw.example.com")
	_, err := grpcEcho(t, insecure.NewCredentials(), grpcAddr, "", "x")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing fleet-session -> %v, want InvalidArgument", err)
	}
}

// TestGatewayGRPCUnknownSession confirms an unknown id is NotFound.
func TestGatewayGRPCUnknownSession(t *testing.T) {
	_, _, grpcAddr := startTestGatewayPlain(t, "http://gw.example.com")
	_, err := grpcEcho(t, insecure.NewCredentials(), grpcAddr, strings.Repeat("b", 64), "x")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown session -> %v, want NotFound", err)
	}
}

// startTestGatewayPlainGRPCBase is startTestGatewayPlain with a public gRPC base
// configured (--public-grpc-url), for Public GRPC URL tests.
func startTestGatewayPlainGRPCBase(t *testing.T, publicBase, grpcBase string) (*Server, string, string) {
	t.Helper()
	s := &Server{
		cfg:       Config{PublicURL: publicBase, PublicGRPCURL: grpcBase, MaxSessions: 64},
		reg:       newRegistry(publicBase, grpcBase, 64, testSigner(t, "")),
		tlsConfig: nil,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	publicLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.ServeListeners(ctx, publicLn, grpcLn) }()
	return s, publicLn.Addr().String(), grpcLn.Addr().String()
}

// TestGatewayPublicGRPCURL verifies the Public GRPC URL contract: a gateway run
// with --public-grpc-url hands <base>/grpc/<publicID> to a daemon that
// negotiates the grpc feature, and withholds it from one that does not (remote
// fleet disabled / legacy fleetd) — so a daemon is never shown a gRPC URL the
// gateway would refuse to route for it.
func TestGatewayPublicGRPCURL(t *testing.T) {
	const grpcBase = "http://gw.example.com:50051"
	_, _, grpcAddr := startTestGatewayPlainGRPCBase(t, "http://gw.example.com", grpcBase)

	// grpc negotiated -> the reply carries the session's gRPC URL.
	reply := dialFleetdGRPCPlain(t, grpcAddr)
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		t.Fatalf("gateway did not negotiate grpc: features=%v", reply.Features)
	}
	want := grpcBase + "/grpc/" + publicIDOf(t, reply.PublicURL)
	if reply.PublicGRPCURL != want {
		t.Fatalf("public grpc url = %q, want %q", reply.PublicGRPCURL, want)
	}

	// No grpc feature requested -> the gRPC URL is withheld.
	legacy := dialFleetdPlain(t, grpcAddr, "")
	if legacy.PublicGRPCURL != "" {
		t.Fatalf("non-negotiated session got a public grpc url: %q", legacy.PublicGRPCURL)
	}
}

// TestGatewayNoPublicGRPCURLWithoutBase confirms a gateway run WITHOUT
// --public-grpc-url never mints a gRPC URL, even for a grpc-negotiating daemon.
func TestGatewayNoPublicGRPCURLWithoutBase(t *testing.T) {
	_, _, grpcAddr := startTestGatewayPlain(t, "http://gw.example.com")

	reply := dialFleetdGRPCPlain(t, grpcAddr)
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		t.Fatalf("gateway did not negotiate grpc: features=%v", reply.Features)
	}
	if reply.PublicGRPCURL != "" {
		t.Fatalf("gateway without a grpc base minted a public grpc url: %q", reply.PublicGRPCURL)
	}
}

// TestGatewayGRPCNotNegotiated confirms the gRPC port rejects (NotFound) a legacy
// session that did not negotiate FeatureGRPC.
func TestGatewayGRPCNotNegotiated(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, _, grpcAddr := startTestGateway(t, cert, "https://gw.example.com")

	reply := dialFleetd(t, grpcAddr, pool, "") // sends no Features
	if len(reply.Features) != 0 {
		t.Fatalf("legacy fleetd should negotiate no features, got %v", reply.Features)
	}
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	_, err := grpcEcho(t, tlsCreds(pool), grpcAddr, id, "x")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("/grpc on a non-negotiated session -> %v, want NotFound", err)
	}
}
