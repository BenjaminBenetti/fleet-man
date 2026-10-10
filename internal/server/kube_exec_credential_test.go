//go:build !nok8s

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func stubKubeExecCredential(t *testing.T, fn func(ctx context.Context, id string, refresh bool) (fleet.KubeExecCredential, error)) {
	t.Helper()
	old := issueKubeExecCredential
	issueKubeExecCredential = fn
	t.Cleanup(func() { issueKubeExecCredential = old })
}

// The shim's RPC is served on the local socket and refused over the
// gateway tunnel and the Armada SSH listener, valid token or not.
func TestIssueKubeExecCredentialIsLocalOnly(t *testing.T) {
	isolateFleetDir(t)
	var gotID string
	var gotRefresh bool
	stubKubeExecCredential(t, func(_ context.Context, id string, refresh bool) (fleet.KubeExecCredential, error) {
		gotID, gotRefresh = id, refresh
		return fleet.KubeExecCredential{ClusterID: id, Server: "https://api.example:6443", BearerToken: "secret-token", Auth: string(kubeexec.AuthToken)}, nil
	})
	req := &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: "prod-abc123", Refresh: true}

	local := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(local, newService())
	reply, err := dialBuf(t, local).IssueKubeExecCredential(context.Background(), req)
	if err != nil {
		t.Fatalf("local socket: %v", err)
	}
	if c := reply.GetCredential(); c.GetBearerToken() != "secret-token" || c.GetServer() != "https://api.example:6443" || gotID != "prod-abc123" || !gotRefresh {
		t.Fatalf("local reply %v (asked for %q refresh %v)", c, gotID, gotRefresh)
	}

	const token = "test-token-123"
	withToken := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	authUnary, authStream := bearerAuthInterceptors(token)
	tunnel := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(tunnel, newService())
	gotID = ""
	if _, err := dialBuf(t, tunnel).IssueKubeExecCredential(withToken, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("over the gateway tunnel with a token: %v, want PermissionDenied", err)
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
	if _, err := fleetgrpc.NewFleetServiceClient(cc).IssueKubeExecCredential(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("over the Armada SSH listener with a token: %v, want PermissionDenied", err)
	}
	if gotID != "" {
		t.Fatal("a remote call reached the handler")
	}
}

func TestIssueKubeExecCredentialErrors(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	if _, err := svc.IssueKubeExecCredential(context.Background(), &fleetgrpc.IssueKubeExecCredentialRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no cluster: %v", err)
	}
	for err, want := range map[error]codes.Code{
		kubernetes.ErrClusterNotFound:                          codes.NotFound,
		kubeexec.ErrKubeconfigMissing:                          codes.FailedPrecondition,
		kubeexec.ErrPluginMissing:                              codes.FailedPrecondition,
		&kubeexec.PluginError{Path: "/bin/aws", ExitCode: 255}: codes.Unauthenticated,
		context.DeadlineExceeded:                               codes.DeadlineExceeded,
		os.ErrPermission:                                       codes.Unavailable,
	} {
		stubKubeExecCredential(t, func(context.Context, string, bool) (fleet.KubeExecCredential, error) {
			return fleet.KubeExecCredential{}, err
		})
		if _, got := svc.IssueKubeExecCredential(context.Background(), &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: "c"}); status.Code(got) != want {
			t.Errorf("%v: code %v, want %v", err, status.Code(got), want)
		}
	}
}

// End to end through a pool over the daemon's own connection records: the
// shim gets the transport of the record's pinned kubeconfig context.
func TestIssueKubeExecCredentialReadsTheConnections(t *testing.T) {
	isolateFleetDir(t)
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://k8s.example:6443", tls-server-name: api.internal}
contexts:
- name: ctx
  context: {cluster: c, user: u, namespace: work}
users:
- name: u
  user: {token: pinned-token, as: alice}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = append(k.Clusters, fleet.KubeCluster{ID: "work-abc123", Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: "work"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pool := kubernetes.NewPool(kubernetes.PoolOptions{})
	t.Cleanup(pool.Close)
	stubKubeExecCredential(t, pool.IssueExecCredential)

	svc := newService()
	reply, err := svc.IssueKubeExecCredential(context.Background(), &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: "work-abc123"})
	if err != nil {
		t.Fatal(err)
	}
	c := reply.GetCredential()
	if c.GetBearerToken() != "pinned-token" || c.GetServer() != "https://k8s.example:6443" || c.GetTlsServerName() != "api.internal" ||
		c.GetImpersonate().GetUser() != "alice" || c.GetClusterId() != "work-abc123" || c.GetAuth() != string(kubeexec.AuthToken) {
		t.Fatalf("issued %v", c)
	}
	if _, err := svc.IssueKubeExecCredential(context.Background(), &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("an unknown connection: %v", err)
	}
}
