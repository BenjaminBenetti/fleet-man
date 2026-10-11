//go:build !nok8s

package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// PodExec is served on the local socket (it reaches the handler, which refuses
// a malformed request) and refused over the gateway tunnel and the Armada SSH
// listener, valid token or not: the kube-exec shim's daemon mode runs on the
// daemon host, and a remote client uses the streaming Exec RPC.
func TestPodExecIsLocalOnly(t *testing.T) {
	isolateFleetDir(t)
	if !localOnlyMethods[fleetgrpc.FleetService_PodExec_FullMethodName] {
		t.Fatal("PodExec must be local-only")
	}
	bad := &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Start{Start: &fleetgrpc.ExecStart{}}}

	// Local socket: no interceptor, so the handler runs and refuses the
	// malformed first frame (proof it was reached, not blocked).
	local := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(local, newService())
	if code := podExecFirstErr(t, dialBuf(t, local), context.Background(), bad); code != codes.InvalidArgument {
		t.Fatalf("local socket: code %v, want InvalidArgument (handler reached)", code)
	}

	const token = "test-token-123"
	withToken := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	authUnary, authStream := bearerAuthInterceptors(token)
	tunnel := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(tunnel, newService())
	if code := podExecFirstErr(t, dialBuf(t, tunnel), withToken, bad); code != codes.PermissionDenied {
		t.Fatalf("over the gateway tunnel with a token: code %v, want PermissionDenied", code)
	}

	l, published := newTestSSHListener(t, token)
	l.Reconcile(true)
	if len(*published) != 1 || (*published)[0].addr == "" {
		t.Fatalf("ssh listener did not start: %+v", *published)
	}
	cc, err := grpc.NewClient("dns:///"+(*published)[0].addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(withToken, 5*time.Second)
	defer cancel()
	if code := podExecFirstErr(t, fleetgrpc.NewFleetServiceClient(cc), ctx, bad); code != codes.PermissionDenied {
		t.Fatalf("over the Armada SSH listener with a token: code %v, want PermissionDenied", code)
	}
}

// podExecFirstErr opens a PodExec stream, sends first, and returns the gRPC
// code of the first error (from Send or the first Recv). A Send that meets
// a stream the server already ended returns io.EOF, whose status is only in
// the next Recv.
func podExecFirstErr(t *testing.T, c fleetgrpc.FleetServiceClient, ctx context.Context, first *fleetgrpc.ExecIn) codes.Code {
	t.Helper()
	stream, err := c.PodExec(ctx)
	if err != nil {
		return status.Code(err)
	}
	if err := stream.Send(first); err != nil && !errors.Is(err, io.EOF) {
		return status.Code(err)
	}
	_ = stream.CloseSend()
	_, err = stream.Recv()
	return status.Code(err)
}

// resolveKubePod finds the Kubernetes instance a pod belongs to by its
// connection and StatefulSet.
func TestResolveKubePod(t *testing.T) {
	isolateFleetDir(t)
	key := names.InstanceKey("daemon12", "alpha", "web", time.Unix(1700000000, 0).UTC())
	sts := names.Instance("alpha", "web", key)
	if err := state.Update(func(st *state.State) error {
		f := st.GetOrCreateFleet("alpha", "https://example.invalid/a.git")
		return f.AddInstance(&fleet.Instance{Name: "web", Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc", KubeKey: key})
	}); err != nil {
		t.Fatal(err)
	}
	inst, err := resolveKubePod("mk-abc", names.Pod(sts))
	if err != nil || inst.Name != "web" {
		t.Fatalf("resolve by pod: %v %v", inst, err)
	}
	if _, err := resolveKubePod("mk-abc", "fi-nope-0"); status.Code(err) != codes.NotFound {
		t.Errorf("unknown pod: %v, want NotFound", err)
	}
	if _, err := resolveKubePod("other-cluster", names.Pod(sts)); status.Code(err) != codes.NotFound {
		t.Errorf("wrong cluster: %v, want NotFound", err)
	}
}
