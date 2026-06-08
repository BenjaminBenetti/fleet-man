package gateway

import (
	"context"
	"io"
	"net"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpc.go serves the dedicated gRPC listener. Unlike the public /mcp listener
// (HTTP/1.1) this speaks native gRPC over HTTP/2 — h2c when cert-less, h2 via ALPN
// under TLS — so a standard L7 gRPC reverse proxy (e.g. Traefik) can front it.
//
// Native gRPC's :path is the method, so the target session cannot ride the path
// the way /mcp/<id> does. Instead the remote client sends the session's public id
// as the `fleet-session` gRPC metadata header; the gateway routes on it and
// TRANSPARENTLY PROXIES the stream to the daemon's own grpc.Server over a TagGRPC
// tunnel stream. It is a grpc-go proxy (UnknownServiceHandler + a raw passthrough
// codec, the well-known grpc-proxy pattern) rather than an HTTP reverse proxy,
// because grpc-go must manage framing and HTTP/2 trailers (grpc-status) on both
// hops — including a trailers-only error response, which an httputil.ReverseProxy
// drops. The bearer token rides in-band (authorization metadata) and is validated
// at the daemon; the gateway only routes.

// grpcSessionHeader is the gRPC metadata key carrying the target session's public
// id (the same id that appears in the /mcp/<id> public URL).
const grpcSessionHeader = "fleet-session"

// proxyStreamDesc describes a fully-streaming method, so the proxy can carry
// unary, server-streaming, client-streaming, and bidi alike.
var proxyStreamDesc = &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}

// newGRPCServer builds the gRPC server for the gRPC listener. grpc.Server speaks
// h2c over a plain listener, or TLS+ALPN h2 when a cert is configured (via creds).
// The fleetd Register method (tunnelServiceDesc) is handled locally; every OTHER
// method hits proxyGRPC (remote-control RPCs routed to a daemon).
func (s *Server) newGRPCServer() *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.ForceServerCodec(tunnel.RawCodec{}),
		grpc.UnknownServiceHandler(s.proxyGRPC),
		grpc.MaxConcurrentStreams(maxConcurrentStreams),
	}
	if s.tlsConfig != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(s.tlsConfig.Clone())))
	}
	srv := grpc.NewServer(opts...)
	srv.RegisterService(&tunnelServiceDesc, s)
	return srv
}

// serveGRPC serves the gRPC server on ln until the server is stopped.
func (s *Server) serveGRPC(srv *grpc.Server, ln net.Listener) error {
	return srv.Serve(ln)
}

// proxyGRPC is the UnknownServiceHandler: it routes by the fleet-session header and
// transparently proxies the stream to the daemon's grpc.Server over the tunnel.
func (s *Server) proxyGRPC(_ any, serverStream grpc.ServerStream) error {
	fullMethod, ok := grpc.MethodFromServerStream(serverStream)
	if !ok {
		return status.Error(codes.Internal, "could not determine method")
	}
	md, _ := metadata.FromIncomingContext(serverStream.Context())
	var id string
	if v := md.Get(grpcSessionHeader); len(v) > 0 {
		id = v[0]
	}
	if id == "" {
		return status.Errorf(codes.InvalidArgument, "missing %s metadata", grpcSessionHeader)
	}
	sess := s.reg.lookup(id)
	if sess == nil || !sess.grpc.Load() {
		return status.Error(codes.NotFound, "unknown session or gRPC not available")
	}
	cc, err := sess.grpcClientConn()
	if err != nil {
		return status.Errorf(codes.Unavailable, "tunnel unavailable: %v", err)
	}

	// Forward all inbound metadata (incl. authorization) to the daemon.
	clientCtx, clientCancel := context.WithCancel(metadata.NewOutgoingContext(serverStream.Context(), md.Copy()))
	defer clientCancel()
	clientStream, err := cc.NewStream(clientCtx, proxyStreamDesc, fullMethod)
	if err != nil {
		return err
	}

	// Pump both directions; whichever side finishes first drives teardown. The
	// daemon's trailers (grpc-status) are copied back so the client sees the real
	// status — including a trailers-only error.
	s2cErr := forwardServerToClient(serverStream, clientStream)
	c2sErr := forwardClientToServer(clientStream, serverStream)
	for range 2 {
		select {
		case err := <-s2cErr:
			if err == io.EOF {
				_ = clientStream.CloseSend()
			} else {
				clientCancel()
				return status.Errorf(codes.Internal, "proxy upstream: %v", err)
			}
		case err := <-c2sErr:
			serverStream.SetTrailer(clientStream.Trailer())
			if err != io.EOF {
				return err
			}
			return nil
		}
	}
	return status.Error(codes.Internal, "gRPC proxy reached an unreachable state")
}

// grpcClientConn returns the session's lazily-built gRPC client connection to the
// daemon's grpc.Server over the tunnel. One per session: its custom dialer opens a
// TagGRPC stream on the live tunnel (re-dialing after a reconnect), and h2c rides
// that stream. The raw codec makes every RPC a byte passthrough.
func (s *session) grpcClientConn() (*grpc.ClientConn, error) {
	s.grpcOnce.Do(func() {
		s.grpcCC, s.grpcCCErr = grpc.NewClient("passthrough:///fleet-tunnel",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
				return s.open(tunnel.TagGRPC)
			}),
			grpc.WithDefaultCallOptions(grpc.ForceCodec(tunnel.RawCodec{})),
		)
	})
	return s.grpcCC, s.grpcCCErr
}

// forwardServerToClient copies inbound (external client) messages to the daemon.
func forwardServerToClient(src grpc.ServerStream, dst grpc.ClientStream) chan error {
	ret := make(chan error, 1)
	go func() {
		f := &tunnel.RawFrame{}
		for {
			if err := src.RecvMsg(f); err != nil {
				ret <- err // io.EOF on a clean client half-close
				return
			}
			if err := dst.SendMsg(f); err != nil {
				ret <- err
				return
			}
		}
	}()
	return ret
}

// forwardClientToServer copies daemon responses back to the external client,
// forwarding the response header once before the first message.
func forwardClientToServer(src grpc.ClientStream, dst grpc.ServerStream) chan error {
	ret := make(chan error, 1)
	go func() {
		f := &tunnel.RawFrame{}
		for i := 0; ; i++ {
			if err := src.RecvMsg(f); err != nil {
				ret <- err // io.EOF on clean end; a status error on rejection
				return
			}
			if i == 0 {
				h, err := src.Header()
				if err != nil {
					ret <- err
					return
				}
				if err := dst.SendHeader(h); err != nil {
					ret <- err
					return
				}
			}
			if err := dst.SendMsg(f); err != nil {
				ret <- err
				return
			}
		}
	}()
	return ret
}
