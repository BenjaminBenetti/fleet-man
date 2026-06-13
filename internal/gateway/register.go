package gateway

import (
	"context"
	"net"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// register.go handles fleetd registration + the reverse tunnel, carried over a gRPC
// bidi stream on the gateway's gRPC port (there is no dedicated TCP control port).
// The stream is wrapped as a net.Conn (tunnel.StreamConn) so the existing handshake
// + yamux machinery is reused verbatim: read RegisterRequest, claim a session,
// write RegisterReply, then run a yamux SERVER session over the same stream and
// open inbound MCP/gRPC streams back down it.
//
// There is still NO registration auth — isolation comes from the unguessable public
// id and the end-to-end bearer token, exactly as before.

// gatewayFeatures lists the optional tunnel features this gateway supports and
// offers in the register handshake. fleetd advertises what IT supports; the
// negotiated set is the intersection.
var gatewayFeatures = []string{tunnel.FeatureGRPC}

// tunnelServiceDesc registers the Register bidi method on the gateway's grpc.Server.
// Its ServiceName/StreamName compose to tunnel.RegisterMethod.
var tunnelServiceDesc = grpc.ServiceDesc{
	ServiceName: "fleet.gateway.Tunnel",
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName:    "Register",
		Handler:       tunnelRegisterHandler,
		ServerStreams: true,
		ClientStreams: true,
	}},
}

func tunnelRegisterHandler(srv any, stream grpc.ServerStream) error {
	return srv.(*Server).handleRegister(stream)
}

// handleRegister wraps the bidi stream as a net.Conn and runs the registration +
// reverse-tunnel lifecycle over it. Returning ends the RPC (and the stream), which
// fleetd observes and reconnects.
//
// Registration is unauthenticated, so each handler first claims a slot from the
// pending-handshake semaphore and sheds the stream (ResourceExhausted) when the cap
// is hit — bounding goroutine/memory growth under a flood of never-completing
// Register streams. The slot is held for the tunnel's whole lifetime (so it also
// counts established tunnels, sized MaxSessions + maxPendingHandshakes).
func (s *Server) handleRegister(ss grpc.ServerStream) error {
	if s.pendingSem != nil {
		select {
		case s.pendingSem <- struct{}{}:
			defer func() { <-s.pendingSem }()
		default:
			return status.Error(codes.ResourceExhausted, "gateway: too many concurrent registrations")
		}
	}

	remote := "unknown"
	if p, ok := peer.FromContext(ss.Context()); ok {
		remote = p.Addr.String()
	}
	conn := tunnel.NewStreamConn(ss, nil)
	return s.bindTunnel(ss.Context(), conn, remote)
}

// bindTunnel performs the register handshake on conn, attaches the resulting yamux
// session to the registry, and serves until the tunnel closes (fleetd gone) or the
// RPC context is cancelled (gateway shutting down). On any failure before bind it
// frees a freshly-reserved slot so it doesn't count against the cap. A failed
// handshake returns an error; a normal tunnel close returns nil.
func (s *Server) bindTunnel(ctx context.Context, conn net.Conn, remote string) error {
	req, err := tunnel.ReadRegisterRequest(conn, registerHandshakeTimeout)
	if err != nil {
		s.log.Warn("gateway: read register", "remote", remote, "err", err)
		return err
	}

	sess, reply, isNew, err := s.reg.claim(req)
	if err != nil {
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Error: err.Error()})
		s.log.Warn("gateway: claim rejected", "remote", remote, "err", err)
		return err
	}

	// Negotiate optional tunnel features (gRPC). Only features BOTH ends support
	// become active; an old fleetd (no Features) negotiates none. A daemon with
	// remote fleet disabled does not request grpc, so none is negotiated and the
	// gateway's gRPC route stays dead for this session — its Public GRPC URL is
	// withheld accordingly.
	reply.Features = tunnel.Negotiate(req.Features, gatewayFeatures)
	grpcOn := tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC)
	sess.grpc.Store(grpcOn)
	if !grpcOn {
		reply.PublicGRPCURL = ""
	}

	// Echo the gateway's build version so fleetd can surface it (over
	// RemoteMcpStatus) to remote TUIs for control-chain version diagnostics.
	reply.GatewayVersion = s.cfg.Version

	if err := tunnel.WriteFrame(conn, reply); err != nil {
		if isNew {
			s.reg.release(sess)
		}
		s.log.Warn("gateway: write register reply", "remote", remote, "err", err)
		return err
	}

	ym, err := tunnel.ServerSession(conn, s.yamuxLog())
	if err != nil {
		if isNew {
			s.reg.release(sess)
		}
		s.log.Warn("gateway: yamux server", "remote", remote, "err", err)
		return err
	}
	s.reg.bind(sess, ym)
	s.log.Info("gateway: tunnel registered", "remote", remote, "public_url", sess.publicURL)

	// Keep serving until the tunnel closes (peer gone, detected by yamux keepalive)
	// or the gateway shuts down. Don't evict here: the session (and its public URL)
	// stays reserved for a grace TTL so a reconnecting fleetd (with its secret)
	// recovers the same URL; the reaper frees it once the TTL elapses.
	select {
	case <-ym.CloseChan():
	case <-ctx.Done():
		_ = ym.Close()
	}
	s.log.Info("gateway: tunnel closed", "remote", remote, "public_url", sess.publicURL)
	return nil
}
