package server

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// remote_auth.go gates the TUNNEL-facing gRPC server (the one reachable through
// the remote gateway) behind the MCP bearer token. Every RPC must carry
// `authorization: Bearer <token>` metadata. The LOCAL unix-socket gRPC server has
// NO interceptor — it is same-user and 0600, so it stays auth-less and unchanged.
//
// The token is the SAME one MCP uses (~/.fleet/mcp.token): one secret gates remote
// access on both paths. The gateway never sees the token as a credential it
// validates — it only routes bytes; the check happens here, at the daemon.

const authMetadataKey = "authorization"

// bearerAuthInterceptors returns unary + stream interceptors that require
// `authorization: Bearer <token>` metadata, compared in constant time.
func bearerAuthInterceptors(token string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	want := []byte("Bearer " + token)

	check := func(ctx context.Context) error {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing credentials")
		}
		vals := md.Get(authMetadataKey)
		if len(vals) == 0 {
			return status.Error(codes.Unauthenticated, "missing authorization")
		}
		if subtle.ConstantTimeCompare([]byte(vals[0]), want) != 1 {
			return status.Error(codes.Unauthenticated, "invalid token")
		}
		return nil
	}

	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := check(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
	return unary, stream
}
