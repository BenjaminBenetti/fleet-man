package gateway

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"

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

// gatewayFeatures lists the optional tunnel features every gateway supports and
// offers in the register handshake. fleetd advertises what IT supports; the
// negotiated set is the intersection. FeatureRegistry joins them only on a
// gateway configured to serve it (see supportedFeatures).
var gatewayFeatures = []string{tunnel.FeatureGRPC, tunnel.FeatureWebhook}

// supportedFeatures is the feature set this gateway offers: gatewayFeatures,
// plus FeatureRegistry when the gateway can serve image pulls (it has a
// session key and an https, path-free public URL; see registryHostFor).
func (s *Server) supportedFeatures() []string {
	if s.reg.registryHost == "" {
		return gatewayFeatures
	}
	return append(slices.Clone(gatewayFeatures), tunnel.FeatureRegistry)
}

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
	if req.Attach != "" {
		return s.bindAttachment(ctx, conn, req, remote)
	}

	sess, reply, isNew, err := s.reg.claim(req)
	if err != nil {
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Error: err.Error()})
		s.log.Warn("gateway: claim rejected", "remote", remote, "err", err)
		return err
	}

	// Negotiate optional tunnel features (gRPC, webhook, registry). Only features
	// BOTH ends support become active; an old fleetd (no Features) negotiates
	// none. A daemon with a traffic kind disabled does not request its feature,
	// so it is not negotiated and the matching gateway route stays dead for this
	// session — its computed public URL is withheld accordingly.
	reply.Features = tunnel.Negotiate(req.Features, s.supportedFeatures())
	grpcOn := tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC)
	sess.grpc.Store(grpcOn)
	if !grpcOn {
		reply.PublicGRPCURL = ""
	}
	webhookOn := tunnel.HasFeature(reply.Features, tunnel.FeatureWebhook)
	sess.webhook.Store(webhookOn)
	if !webhookOn {
		reply.PublicWebhookURL = ""
	}
	registryOn := tunnel.HasFeature(reply.Features, tunnel.FeatureRegistry)
	sess.registry.Store(registryOn)
	if !registryOn {
		reply.PublicRegistry = ""
		if tunnel.HasFeature(req.Features, tunnel.FeatureRegistry) {
			// Asked for and not offered: say why, so the daemon's settings
			// can show it instead of guessing.
			reply.RegistryOff = s.registryOff
		}
		// A re-registration that dropped the feature (Enable Image Registry
		// turned off) leaves no use for a registry connection.
		sess.closeRegistry()
	}
	sess.tagged.Store(tunnel.Tagged(reply.Features))

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

// errNoAttachTarget refuses a registry attach that names no bound session with
// the registry feature negotiated. One message for every case, so the reply
// says nothing about which secrets exist.
var errNoAttachTarget = errors.New("gateway: no registry session for this attach")

// bindAttachment binds an auxiliary registry connection (RegisterRequest.Attach
// == tunnel.AttachRegistry) to the session its SessionID names, and serves it
// until it closes or the gateway shuts down. It is the session's registryYm
// slot: one per session (a newer attach supersedes an older one), never the
// main tunnel, and never counted against MaxSessions. The gateway opens
// registry streams on it while it is live and on the main tunnel otherwise.
//
// The secret is the only credential, as for a reclaim: whoever holds it owns
// the session anyway. The connection runs the registry yamux config (4 MiB
// stream window, 60s keepalive timeout).
func (s *Server) bindAttachment(ctx context.Context, conn net.Conn, req tunnel.RegisterRequest, remote string) error {
	if req.Attach != tunnel.AttachRegistry {
		err := errors.New("gateway: unknown attach role " + strconv.Quote(req.Attach))
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Error: err.Error()})
		s.log.Warn("gateway: attach rejected", "remote", remote, "err", err)
		return err
	}
	sess := s.reg.attachTarget(req.SessionID)
	if sess == nil || !sess.registry.Load() {
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Error: errNoAttachTarget.Error()})
		s.log.Warn("gateway: attach rejected", "remote", remote, "err", errNoAttachTarget)
		return errNoAttachTarget
	}
	reply := tunnel.RegisterReply{
		Features:       []string{tunnel.FeatureRegistry},
		GatewayVersion: s.cfg.Version,
	}
	if err := tunnel.WriteFrame(conn, reply); err != nil {
		s.log.Warn("gateway: write attach reply", "remote", remote, "err", err)
		return err
	}
	ym, err := tunnel.RegistryServerSession(conn, s.yamuxLog())
	if err != nil {
		s.log.Warn("gateway: yamux server (registry)", "remote", remote, "err", err)
		return err
	}
	if !s.reg.attachRegistry(sess, ym) {
		s.log.Warn("gateway: attach rejected", "remote", remote, "err", errNoAttachTarget)
		return errNoAttachTarget
	}
	s.log.Info("gateway: registry connection attached", "remote", remote, "public_url", sess.publicURL)

	select {
	case <-ym.CloseChan():
	case <-ctx.Done():
		_ = ym.Close()
	}
	sess.clearRegistryYamux(ym)
	s.log.Info("gateway: registry connection closed", "remote", remote, "public_url", sess.publicURL)
	return nil
}
