package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestGatewayRegisterFloodShed verifies the gateway sheds excess concurrent Register
// streams with ResourceExhausted once the pending-handshake semaphore is full,
// instead of letting unauthenticated streams pile up (the DoS bound that replaced
// the old control-port load-shedding accept loop).
func TestGatewayRegisterFloodShed(t *testing.T) {
	s := &Server{
		cfg:        Config{PublicURL: "http://gw.example.com", MaxSessions: 64},
		reg:        newRegistry("http://gw.example.com", 64, testSigner(t, "")),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		pendingSem: make(chan struct{}, 2), // tiny cap for the test
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := s.newGRPCServer()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	openRegister := func() (grpc.ClientStream, *grpc.ClientConn) {
		cc, err := grpc.NewClient("dns:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		stream, err := cc.NewStream(context.Background(),
			&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
			tunnel.RegisterMethod, grpc.ForceCodec(tunnel.RawCodec{}))
		if err != nil {
			_ = cc.Close()
			t.Fatalf("new stream: %v", err)
		}
		return stream, cc
	}

	// Two streams occupy the cap; their handlers block in the handshake read (they
	// never send a RegisterRequest), holding both semaphore slots.
	for range 2 {
		_, cc := openRegister()
		t.Cleanup(func() { _ = cc.Close() })
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(s.pendingSem) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("handlers never filled the pending-handshake cap")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A third Register stream is shed.
	st, cc := openRegister()
	t.Cleanup(func() { _ = cc.Close() })
	if err := st.RecvMsg(&tunnel.RawFrame{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("third register stream: want ResourceExhausted, got %v", err)
	}
}

// TestGatewayReconnectAfterDrop verifies a LIVE tunnel that drops can be reclaimed
// over a fresh Register stream with the same secret, recovering the same public URL
// — the reconnect path, exercised over the real StreamConn transport.
func TestGatewayReconnectAfterDrop(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, _, grpcAddr := startTestGateway(t, cert, "https://gw.example.com")

	// Register and bring up yamux over the Register stream.
	conn1 := openRegisterStream(t, grpcAddr, tlsCreds(pool))
	r1, err := tunnel.Handshake(conn1, tunnel.RegisterRequest{ClientVersion: "v"}, 5*time.Second)
	if err != nil || r1.Error != "" {
		t.Fatalf("handshake 1: err=%v reply.Error=%q", err, r1.Error)
	}
	sess1, err := tunnel.ClientSession(conn1, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	waitRegistered(t, s, publicIDOf(t, r1.PublicURL))

	// Drop the live tunnel.
	_ = sess1.Close()
	_ = conn1.Close()

	// Reconnect over a FRESH Register stream, presenting the secret -> same URL.
	conn2 := openRegisterStream(t, grpcAddr, tlsCreds(pool))
	r2, err := tunnel.Handshake(conn2, tunnel.RegisterRequest{SessionID: r1.SessionID, ClientVersion: "v"}, 5*time.Second)
	if err != nil || r2.Error != "" {
		t.Fatalf("handshake 2: err=%v reply.Error=%q", err, r2.Error)
	}
	if r2.PublicURL != r1.PublicURL {
		t.Fatalf("reconnect public URL = %q, want %q (sticky)", r2.PublicURL, r1.PublicURL)
	}
	if r2.SessionID != r1.SessionID {
		t.Fatalf("reconnect secret changed %q -> %q", r1.SessionID, r2.SessionID)
	}
}

// TestGatewayRestartStableURL exercises issue #120 over the real Register
// transport: a daemon that registered against one gateway reconnects to a
// RESTARTED gateway (a fresh instance sharing the --session-key) presenting the
// session token from its first reply, and recovers the SAME public URL. A
// restarted gateway with a DIFFERENT key hands out a fresh URL instead.
func TestGatewayRestartStableURL(t *testing.T) {
	cert, pool := genTestTLS(t)
	const key = "stable-session-key"

	// First boot: register fresh.
	_, _, grpcAddr1 := startTestGatewayKeyed(t, cert, "https://gw.example.com", key)
	conn1 := openRegisterStream(t, grpcAddr1, tlsCreds(pool))
	r1, err := tunnel.Handshake(conn1, tunnel.RegisterRequest{ClientVersion: "v"}, 5*time.Second)
	if err != nil || r1.Error != "" {
		t.Fatalf("handshake 1: err=%v reply.Error=%q", err, r1.Error)
	}
	if r1.SessionToken == "" {
		t.Fatal("registration must return a session token")
	}
	_ = conn1.Close()

	// "Restart": an entirely new gateway instance (empty registry), same key.
	_, _, grpcAddr2 := startTestGatewayKeyed(t, cert, "https://gw.example.com", key)
	conn2 := openRegisterStream(t, grpcAddr2, tlsCreds(pool))
	r2, err := tunnel.Handshake(conn2, tunnel.RegisterRequest{
		SessionID:     r1.SessionID,
		SessionToken:  r1.SessionToken,
		ClientVersion: "v",
	}, 5*time.Second)
	if err != nil || r2.Error != "" {
		t.Fatalf("handshake 2: err=%v reply.Error=%q", err, r2.Error)
	}
	if r2.PublicURL != r1.PublicURL || r2.SessionID != r1.SessionID {
		t.Fatalf("session must survive the restart: url %q -> %q", r1.PublicURL, r2.PublicURL)
	}

	// A restart with a DIFFERENT key cannot verify the token: fresh URL.
	_, _, grpcAddr3 := startTestGatewayKeyed(t, cert, "https://gw.example.com", "rotated-key")
	conn3 := openRegisterStream(t, grpcAddr3, tlsCreds(pool))
	r3, err := tunnel.Handshake(conn3, tunnel.RegisterRequest{
		SessionID:     r1.SessionID,
		SessionToken:  r1.SessionToken,
		ClientVersion: "v",
	}, 5*time.Second)
	if err != nil || r3.Error != "" {
		t.Fatalf("handshake 3: err=%v reply.Error=%q", err, r3.Error)
	}
	if r3.PublicURL == r1.PublicURL {
		t.Fatal("a gateway with a different key must NOT honor the token")
	}
}
