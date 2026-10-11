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

// localOnlyMethods are RPCs the tunnel-facing server must REFUSE even with a
// valid token: they expose data that belongs to the user's own machine and has
// no meaning on (and must not leak to) a remote caller. The fleet-armada
// registry holds the bearer tokens of OTHER remote fleets the local user
// registered — serving it over the tunnel would let anyone holding THIS
// daemon's token harvest or overwrite the user's whole registry of other
// fleets (lateral movement). The registry's own RPCs only ever ride the
// client's LOCAL dial (fleetclient.DialLocal), so denying them remotely never
// breaks a legitimate caller. Keys are gRPC full method names.
var localOnlyMethods = map[string]bool{
	"/fleetgrpc.FleetService/GetArmada":           true,
	"/fleetgrpc.FleetService/SetArmada":           true,
	"/fleetgrpc.FleetService/ResolveArmadaRemote": true, // hands out a remote's bearer token + drives the user's ssh
	"/fleetgrpc.FleetService/TrustSSHHostKey":     true, // writes the user's known_hosts + hands out a remote's bearer token
	// Internal plumbing, not a limit on remote control: it hands the
	// daemon-host `fleet kube-exec` shim a live cluster credential. A remote
	// client gets the same capability (every shell, pane and command in a
	// Kubernetes pod) through the streaming Exec RPC, which fleetd serves over
	// its own connection, so cluster tokens never leave the daemon host. This
	// is the one exception the remote full-control rule allows.
	"/fleetgrpc.FleetService/IssueKubeExecCredential": true,
	// Same reason as IssueKubeExecCredential: PodExec serves the daemon-host
	// `fleet kube-exec` shim's daemon mode, one command in a pod on the
	// daemon's own connection. A remote client runs the same command through
	// the streaming Exec RPC, so a cluster is never reached on a remote
	// caller's behalf through this path.
	"/fleetgrpc.FleetService/PodExec": true,
	// The editor endpoint records (armada.proto) are the user's own registry
	// of remote daemons, like the armada's, and may hold another daemon's
	// bearer token. `fleet code` and ssh-proxy only ever send them over the
	// local dial; a remote client opening an editor records its own remote
	// on its own daemon.
	"/fleetgrpc.FleetService/SaveEditorEndpoint":    true,
	"/fleetgrpc.FleetService/ResolveEditorEndpoint": true,
}

// remoteCallerKey marks the context of an RPC that came through the
// token-gated listener (a gateway tunnel, Armada SSH, FLEET_SERVER) rather
// than the local unix socket.
type remoteCallerKey struct{}

// remoteCaller reports whether the RPC whose context ctx is came through
// the token-gated listener: its caller is not on the daemon's host (as far
// as fleetd can tell), so what it opens there (an editor) cannot reach the
// daemon host's docker.
func remoteCaller(ctx context.Context) bool {
	remote, _ := ctx.Value(remoteCallerKey{}).(bool)
	return remote
}

// remoteServerStream carries the remote-caller mark on a stream's context.
type remoteServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s remoteServerStream) Context() context.Context { return s.ctx }

// bearerAuthInterceptors returns unary + stream interceptors that require
// `authorization: Bearer <token>` metadata, compared in constant time, and
// refuse the local-only methods regardless of token.
func bearerAuthInterceptors(token string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	want := []byte("Bearer " + token)

	check := func(ctx context.Context, fullMethod string) error {
		if localOnlyMethods[fullMethod] {
			return status.Error(codes.PermissionDenied, "this method is local-only and not available over the gateway")
		}
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

	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, remoteCallerKey{}, true), req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := check(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, remoteServerStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), remoteCallerKey{}, true)})
	}
	return unary, stream
}
