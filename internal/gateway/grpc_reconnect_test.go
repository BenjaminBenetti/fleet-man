package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpc_reconnect_test.go covers the tunnel-bounce behaviour of the gRPC proxy
// (issue #141 note 4): the session's cached grpc.ClientConn must not stay stuck
// in connect backoff after the daemon reconnects, and tunnel-transport failures
// must surface to remote clients as a clear retryable status rather than the raw
// "session shutdown" dial error.

// dialFleetdGRPCDroppable registers a gRPC-negotiating fake daemon (raw-echo
// grpc.Server served over TagGRPC streams, as in grpc_route_test.go) over a
// plaintext Register stream, optionally reclaiming sessionID, and returns the
// gateway's reply plus a drop function that tears the tunnel down on demand —
// simulating a daemon bounce mid-test instead of only at cleanup.
func dialFleetdGRPCDroppable(t *testing.T, grpcAddr, sessionID string) (tunnel.RegisterReply, func()) {
	t.Helper()
	conn := openRegisterStream(t, grpcAddr, insecure.NewCredentials())
	if err := tunnel.WriteFrame(conn, tunnel.RegisterRequest{SessionID: sessionID, Features: []string{tunnel.FeatureGRPC}}); err != nil {
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

	var once sync.Once
	drop := func() {
		once.Do(func() {
			srv.Stop()
			_ = lis.Close()
			_ = sess.Close()
			_ = conn.Close()
		})
	}
	t.Cleanup(drop)
	return reply, drop
}

// grpcEchoDeadline is grpcEcho with a per-RPC deadline, so tests can assert the
// proxy resolves (or fails) well within a bound.
func grpcEchoDeadline(t *testing.T, grpcAddr, id, payload string, timeout time.Duration) (string, error) {
	t.Helper()
	conn, err := grpc.NewClient("dns:///"+grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(tunnel.RawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc client: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, grpcSessionHeader, id)
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

// waitTunnelLive waits until the session's bound tunnel exists and is open.
func waitTunnelLive(t *testing.T, sess *session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ym := sess.currentYamux(); ym != nil && !ym.IsClosed() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("tunnel never became live")
}

// waitTunnelClosed waits until the gateway has observed the session's tunnel die.
func waitTunnelClosed(t *testing.T, sess *session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ym := sess.currentYamux(); ym != nil && ym.IsClosed() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("gateway never observed the tunnel close")
}

// driveToArmedBackoff forces cc to dial (and fail) against the session's dead
// tunnel until it lands in TRANSIENT_FAILURE — i.e. with a connect backoff of at
// least 0.8s armed (1s base, −20% jitter). In production the remote TUI's polling
// RPCs trigger that dial; here cc.Connect() stands in for them. Note pick_first
// holds TRANSIENT_FAILURE sticky (no further state flaps to observe) and only
// re-dials on demand, so one observed failure is exactly "backoff armed". The
// armed backoff is what makes the short-deadline RPC after rebind discriminating:
// without the reset it cannot fire in time.
func driveToArmedBackoff(t *testing.T, cc *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for st := cc.GetState(); st != connectivity.TransientFailure; st = cc.GetState() {
		cc.Connect() // exit idle; no-op while already connecting
		if !cc.WaitForStateChange(ctx, st) {
			t.Fatalf("conn stuck in %v waiting for TRANSIENT_FAILURE", st)
		}
	}
}

// TestGatewayGRPCRecoversImmediatelyAfterRebind is the tunnel-bounce regression
// test: an RPC builds the session's cached client conn; the tunnel drops and the
// conn's backoff is armed against the dead tunnel; the daemon reconnects (same
// secret → same session). An immediate RPC with a deadline far below the armed
// backoff must succeed — proving bind() resets the conn's backoff and
// WaitForReady rides the redial. Without the fix the conn keeps fail-fasting
// (or, with WaitForReady alone, burns the deadline waiting out the backoff).
func TestGatewayGRPCRecoversImmediatelyAfterRebind(t *testing.T) {
	s, _, grpcAddr := startTestGatewayPlain(t, "http://gw.example.com")

	reply, drop := dialFleetdGRPCDroppable(t, grpcAddr, "")
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	// Baseline RPC: builds + connects the session's cached client conn.
	if got, err := grpcEchoDeadline(t, grpcAddr, id, "one", 5*time.Second); err != nil || got != "one" {
		t.Fatalf("baseline echo = (%q, %v), want (one, nil)", got, err)
	}

	// Bounce the daemon and arm the cached conn's connect backoff against the
	// now-dead tunnel.
	drop()
	sess := s.reg.lookup(id)
	if sess == nil {
		t.Fatal("session vanished after drop (should stay reserved for the TTL)")
	}
	waitTunnelClosed(t, sess)
	cc, err := sess.grpcClientConn()
	if err != nil {
		t.Fatalf("client conn: %v", err)
	}
	driveToArmedBackoff(t, cc)

	// The daemon reconnects with its secret and recovers the same session.
	reply2, _ := dialFleetdGRPCDroppable(t, grpcAddr, reply.SessionID)
	if reply2.PublicURL != reply.PublicURL {
		t.Fatalf("reclaim changed the URL: %q -> %q", reply.PublicURL, reply2.PublicURL)
	}
	waitTunnelLive(t, sess)

	// Immediate RPC with a deadline well under the ≥0.8s armed backoff: only the
	// backoff reset (+ WaitForReady covering the redial race) makes this pass.
	if got, err := grpcEchoDeadline(t, grpcAddr, id, "two", 400*time.Millisecond); err != nil || got != "two" {
		t.Fatalf("post-rebind echo = (%q, %v), want immediate success", got, err)
	}
}

// TestGatewayGRPCDroppedTunnelFriendlyUnavailable: while the tunnel is down with
// no replacement bound, remote RPCs must fail FAST with the friendly retryable
// status — not the raw yamux "session shutdown" dial error, and not a
// DeadlineExceeded after burning the caller's whole deadline.
func TestGatewayGRPCDroppedTunnelFriendlyUnavailable(t *testing.T) {
	s, _, grpcAddr := startTestGatewayPlain(t, "http://gw.example.com")

	reply, drop := dialFleetdGRPCDroppable(t, grpcAddr, "")
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	// Prime the proxy's cached client conn over the live tunnel.
	if got, err := grpcEchoDeadline(t, grpcAddr, id, "up", 5*time.Second); err != nil || got != "up" {
		t.Fatalf("baseline echo = (%q, %v), want (up, nil)", got, err)
	}

	drop()
	sess := s.reg.lookup(id)
	if sess == nil {
		t.Fatal("session vanished after drop (should stay reserved for the TTL)")
	}
	waitTunnelClosed(t, sess)

	start := time.Now()
	_, err := grpcEchoDeadline(t, grpcAddr, id, "down", 3*time.Second)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		t.Fatalf("dropped tunnel -> %v, want codes.Unavailable", err)
	}
	if !strings.Contains(st.Message(), "reconnecting") {
		t.Fatalf("status message %q, want the friendly reconnecting hint", st.Message())
	}
	if strings.Contains(st.Message(), "session shutdown") {
		t.Fatalf("raw transport error leaked to the client: %q", st.Message())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dropped-tunnel RPC took %v; want fail-fast, not a burned deadline", elapsed)
	}
}

// TestTranslateTunnelErr pins the classification: tunnel-transport failures (by
// sentinel or by the text grpc-go flattens dial errors into) become the friendly
// Unavailable, while daemon statuses and unrelated errors pass through untouched.
func TestTranslateTunnelErr(t *testing.T) {
	tunnelErrs := []error{
		errNoTunnel,
		yamux.ErrSessionShutdown,
		fmt.Errorf("open stream: %w", yamux.ErrSessionShutdown),
		status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: session shutdown"`),
		status.Error(codes.DeadlineExceeded, `latest balancer error: connection error: desc = "transport: Error while dialing: gateway: session has no live tunnel"`),
	}
	for _, err := range tunnelErrs {
		if got := translateTunnelErr(err); got != errTunnelReconnecting {
			t.Errorf("translateTunnelErr(%v) = %v, want errTunnelReconnecting", err, got)
		}
	}

	passThrough := []error{
		nil,
		io.EOF,
		status.Error(codes.PermissionDenied, "invalid token"), // the daemon's own status
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		fmt.Errorf("some unrelated proxy error"),
	}
	for _, err := range passThrough {
		if got := translateTunnelErr(err); got != err {
			t.Errorf("translateTunnelErr(%v) = %v, want it unchanged", err, got)
		}
	}
}

// TestRegistryReapClosesCachedGRPCConn: reaping a session must close its cached
// gRPC client conn — the session object is never resurrected (a reclaim past the
// TTL mints a new one), so an unclosed conn would redial the dead tunnel forever.
func TestRegistryReapClosesCachedGRPCConn(t *testing.T) {
	r := newRegistry("https://gw", "", 16, testSigner(t, ""))
	const ttl = 5 * time.Minute

	s, _, _, err := r.claim(tunnel.RegisterRequest{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ym := fakeTunnel(t)
	r.bind(s, ym)
	cc, err := s.grpcClientConn()
	if err != nil {
		t.Fatalf("client conn: %v", err)
	}

	_ = ym.Close()
	now := time.Now()
	r.reap(now, ttl)            // first observation: stamps closedAt, keeps the session
	r.reap(now.Add(2*ttl), ttl) // past the grace TTL: evicted
	if r.lookup(s.publicID) != nil {
		t.Fatal("session should be reaped after the TTL")
	}
	if got := cc.GetState(); got != connectivity.Shutdown {
		t.Fatalf("reaped session's client conn state = %v, want Shutdown (closed)", got)
	}
}
