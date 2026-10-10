//go:build !nok8s

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_remote_test.go holds remote Fleet to full control: every kube RPC and
// every KubeCluster field works the same over the local socket, the gateway
// tunnel (the production gateway, remote.Manager and token-gated server) and
// the Armada SSH listener (sshlisten.go, the port an `ssh -L` forward
// reaches), and no reply on any of them carries kubeconfig contents, a token
// or the gateway credential.

const (
	pluginToken   = "plugin-SECRET-token-777"
	pluginStderr  = "refresh token rt-SECRET-555 is stale"
	gatewaySecret = "gw-cred-SECRET-999"
)

// secrets are the values no kube reply may carry.
var kubeSecrets = []string{fakeKubeToken, pluginToken, "rt-SECRET-555", gatewaySecret}

// noSecrets fails the test when msg carries a secret.
func noSecrets(t *testing.T, transport string, msg proto.Message) {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range kubeSecrets {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("%s: a %T reply carries the secret %q", transport, msg, s)
		}
	}
}

// remoteKubeTransports serves one service three ways and returns a client
// for each: the local socket's server, the gateway tunnel, and the SSH
// listener. All three share the bearer token in ~/.fleet/mcp.token.
func remoteKubeTransports(t *testing.T, svc *service) map[string]fleetgrpc.FleetServiceClient {
	t.Helper()
	clients := map[string]fleetgrpc.FleetServiceClient{"local": dialBuf(t, newLocalServer(svc))}

	dial, token := grpcStackFor(t, svc)
	clients["gateway"] = fleetgrpc.NewFleetServiceClient(dial(true))

	l := &sshListener{
		newServer: func() (*grpc.Server, error) {
			tok, err := loadOrCreateMCPToken()
			if err != nil {
				return nil, err
			}
			authUnary, authStream := bearerAuthInterceptors(tok)
			gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
			fleetgrpc.RegisterFleetServiceServer(gs, svc)
			return gs, nil
		},
		publish: func(string, string) {},
	}
	l.Reconcile(true)
	t.Cleanup(l.Stop)
	// What a remote's discovery reads over SSH: the port and the token.
	port, err := os.ReadFile(fleetpaths.SSHPortPath())
	if err != nil {
		t.Fatalf("ssh.port: %v", err)
	}
	if _, err := strconv.Atoi(string(port)); err != nil {
		t.Fatalf("ssh.port = %q", port)
	}
	conn, err := grpc.NewClient("dns:///"+net.JoinHostPort("127.0.0.1", string(port)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerCreds(token)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	clients["armada-ssh"] = fleetgrpc.NewFleetServiceClient(conn)
	return clients
}

type bearerCreds string

func (b bearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearerCreds) RequireTransportSecurity() bool { return false }

// parityResult is what one transport's run of the suite produced.
type parityResult struct {
	record     *fleetgrpc.KubeCluster // the client view, its per-transport fields cleared
	testState  fleetgrpc.KubeClusterState
	checkNames string
	pruneOwner string
	manifest   string
	login      []*fleetgrpc.KubeDeviceCode
}

// The same suite over each transport gives the same results: a remote client
// adds a connection from a non-default kubeconfig on the daemon host, sets
// every field (allow_privileged, extra_path and plugin_env included), tests,
// prunes, creates a namespace, logs in on the daemon and removes it.
func TestKubeRPCsHaveRemoteParity(t *testing.T) {
	f := newKubeFixture(t)
	f.real["plugin-ctx"] = true
	startHub(t, f.svc)
	clients := remoteKubeTransports(t, f.svc)
	for _, m := range []string{
		fleetgrpc.FleetService_ListKubeContexts_FullMethodName, fleetgrpc.FleetService_ListKubeClusters_FullMethodName,
		fleetgrpc.FleetService_AddKubeCluster_FullMethodName, fleetgrpc.FleetService_UpdateKubeCluster_FullMethodName,
		fleetgrpc.FleetService_RemoveKubeCluster_FullMethodName, fleetgrpc.FleetService_TestKubeCluster_FullMethodName,
		fleetgrpc.FleetService_PruneKubeCluster_FullMethodName, fleetgrpc.FleetService_LoginKubeCluster_FullMethodName,
		fleetgrpc.FleetService_CreateKubeNamespace_FullMethodName,
	} {
		if localOnlyMethods[m] {
			t.Errorf("%s is local-only: remote Fleet must have it", m)
		}
	}
	// The one exception: the daemon-host kube-exec shim's credential, which
	// remote clients do not need (they exec through Exec).
	if !localOnlyMethods[fleetgrpc.FleetService_IssueKubeExecCredential_FullMethodName] {
		t.Error("IssueKubeExecCredential must be local-only")
	}
	// PodExec (the kube-exec shim's daemon mode) joins it for the same reason.
	if !localOnlyMethods[fleetgrpc.FleetService_PodExec_FullMethodName] {
		t.Error("PodExec must be local-only")
	}
	stubKubeExecCredential(t, func(_ context.Context, id string, _ bool) (fleet.KubeExecCredential, error) {
		return fleet.KubeExecCredential{ClusterID: id, BearerToken: "shim-only"}, nil
	})
	for name, c := range clients {
		reply, err := c.IssueKubeExecCredential(context.Background(), &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: "any"})
		switch {
		case name == "local" && (err != nil || reply.GetCredential().GetBearerToken() != "shim-only"):
			t.Errorf("IssueKubeExecCredential on the local socket: %v %v", reply, err)
		case name != "local" && status.Code(err) != codes.PermissionDenied:
			t.Errorf("IssueKubeExecCredential over %s: %v, want PermissionDenied", name, err)
		}
	}

	results := map[string]parityResult{}
	for _, name := range []string{"local", "gateway", "armada-ssh"} {
		t.Run(name, func(t *testing.T) {
			results[name] = runKubeParitySuite(t, f, name, clients[name])
		})
	}
	local := results["local"]
	for _, name := range []string{"gateway", "armada-ssh"} {
		got := results[name]
		if !proto.Equal(got.record, local.record) {
			t.Errorf("%s stored\n %v\nlocal stored\n %v", name, got.record, local.record)
		}
		if got.testState != local.testState || got.checkNames != local.checkNames || got.pruneOwner != local.pruneOwner || got.manifest != local.manifest {
			t.Errorf("%s = %+v\nlocal = %+v", name, got, local)
		}
		if len(got.login) != 1 || !proto.Equal(got.login[0], local.login[0]) {
			t.Errorf("%s login relayed %v, local %v", name, got.login, local.login)
		}
	}
}

func runKubeParitySuite(t *testing.T, f *kubeFixture, name string, c fleetgrpc.FleetServiceClient) parityResult {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var res parityResult

	// A kubeconfig of this transport's own, away from fleetd's default.
	dir := filepath.Join(f.home, "kubeconfigs", name)
	kubeconfig := writeKubeconfigFile(t, filepath.Join(dir, "config"), "minikube", `
- name: minikube
  context: {cluster: minikube, user: minikube}
- name: token-ctx
  context: {cluster: prod, user: token-user}
- name: plugin-ctx
  context: {cluster: minikube, user: plugin}
`, fmt.Sprintf(`
- name: minikube
  user: {client-certificate-data: Y2VydA==, client-key-data: a2V5}
- name: token-user
  user: {token: %s}
- name: plugin
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: login-plugin, interactiveMode: IfAvailable}}
`, fakeKubeToken))
	pluginDir := filepath.Join(dir, "sdk", "bin")
	writePluginScript(t, filepath.Join(pluginDir, "login-plugin"), fmt.Sprintf(`echo %q >&2
echo "To sign in, use a web browser to open the page https://microsoft.com/devicelogin and enter the code PAR1TY%s to authenticate." >&2
printf '%%s' '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"%s"}}'`, pluginStderr, "X", pluginToken))

	contexts, err := c.ListKubeContexts(ctx, &fleetgrpc.ListKubeContextsRequest{KubeconfigPaths: []string{kubeconfig}})
	if err != nil {
		t.Fatalf("ListKubeContexts: %v", err)
	}
	noSecrets(t, name, contexts)
	if len(contexts.GetContexts()) != 3 {
		t.Fatalf("contexts = %v", contexts.GetContexts())
	}

	added, err := c.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{
		Context: "minikube", KubeconfigPaths: []string{kubeconfig}, DisplayName: "parity-" + name, Namespace: "fleet",
	}})
	if err != nil {
		t.Fatalf("AddKubeCluster: %v", err)
	}
	noSecrets(t, name, added)
	id := added.GetCluster().GetCluster().GetId()
	// The gateway pull credential is stored on the daemon host only.
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Find(id).Image.GatewayCredential = gatewaySecret
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	every := everyKubeField(pluginDir)
	every.KubeconfigPaths, every.DisplayName = []string{kubeconfig}, "parity-"+name
	updated, err := c.UpdateKubeCluster(ctx, &fleetgrpc.UpdateKubeClusterRequest{Id: id, Cluster: every, UpdateMask: &fieldmaskpb.FieldMask{Paths: settableKubePaths()}})
	if err != nil {
		t.Fatalf("UpdateKubeCluster(every field): %v", err)
	}
	noSecrets(t, name, updated)
	if got := clientView(updated.GetCluster().GetCluster()); !proto.Equal(got, every) {
		t.Errorf("updated record\n got %v\nwant %v", got, every)
	}
	if !updated.GetCluster().GetCluster().GetAllowPrivileged() || updated.GetCluster().GetCluster().GetExtraPath()[0] != pluginDir {
		t.Errorf("allow_privileged / extra_path not set: %v", updated.GetCluster().GetCluster())
	}

	list, err := c.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	noSecrets(t, name, list)
	for _, st := range list.GetClusters() {
		if st.GetCluster().GetId() == id {
			rec := clientView(st.GetCluster())
			rec.KubeconfigPaths, rec.DisplayName, rec.ExtraPath = nil, "", nil
			res.record = rec
		}
	}
	if res.record == nil {
		t.Fatalf("%s missing from %v", id, list)
	}

	stream, err := c.TestKubeCluster(ctx, &fleetgrpc.TestKubeClusterRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("TestKubeCluster: %v", err)
		}
		noSecrets(t, name, ev)
		if r := ev.GetResult(); r != nil {
			res.testState = r.GetState()
			var names []string
			for _, ch := range r.GetChecks() {
				names = append(names, ch.GetName()+"="+ch.GetResult().String())
			}
			res.checkNames = strings.Join(names, " ")
		}
	}

	prune, err := c.PruneKubeCluster(ctx, &fleetgrpc.PruneKubeClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("PruneKubeCluster: %v", err)
	}
	noSecrets(t, name, prune)
	res.pruneOwner = prune.GetOwner()

	ns, err := c.CreateKubeNamespace(ctx, &fleetgrpc.CreateKubeNamespaceRequest{Id: id, Namespace: "fleet-parity", DryRun: true})
	if err != nil {
		t.Fatalf("CreateKubeNamespace: %v", err)
	}
	res.manifest = ns.GetManifest()

	// [Run login on daemon]: the plugin runs where the kubeconfig is, and
	// only its device code comes back.
	login, err := c.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{
		Context: "plugin-ctx", KubeconfigPaths: []string{kubeconfig}, DisplayName: "login-" + name, ExtraPath: []string{pluginDir},
	}})
	if err != nil {
		t.Fatalf("AddKubeCluster(plugin): %v", err)
	}
	lstream, err := c.LoginKubeCluster(ctx, &fleetgrpc.LoginKubeClusterRequest{Id: login.GetCluster().GetCluster().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	gotResult := false
	for {
		ev, err := lstream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("LoginKubeCluster: %v", err)
		}
		noSecrets(t, name, ev)
		if dc := ev.GetDeviceCode(); dc != nil {
			res.login = append(res.login, dc)
		}
		if ev.GetResult() != nil {
			gotResult = true
		}
	}
	if !gotResult || len(res.login) != 1 || res.login[0].GetUrl() != "https://microsoft.com/devicelogin" || res.login[0].GetCode() != "PAR1TYX" {
		t.Errorf("login relayed %v (result %v)", res.login, gotResult)
	}

	for _, rid := range []string{id, login.GetCluster().GetCluster().GetId()} {
		removed, err := c.RemoveKubeCluster(ctx, &fleetgrpc.RemoveKubeClusterRequest{Id: rid})
		if err != nil || !removed.GetRemoved() {
			t.Fatalf("RemoveKubeCluster(%s) = %v, %v", rid, removed, err)
		}
	}
	return res
}

// Without the bearer token neither remote serves a kube RPC.
func TestKubeRPCsNeedTheTokenRemotely(t *testing.T) {
	f := newKubeFixture(t)
	startHub(t, f.svc)
	dial, _ := grpcStackFor(t, f.svc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := fleetgrpc.NewFleetServiceClient(dial(false)).ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{}); err == nil {
		t.Error("the gateway served ListKubeClusters without the token")
	}
	authUnary, authStream := bearerAuthInterceptors("the-token")
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(gs, f.svc)
	client := dialBuf(t, gs)
	if _, err := client.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube"}}); err == nil {
		t.Error("the SSH listener's server added a connection without the token")
	}
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer the-token")
	if _, err := client.AddKubeCluster(authed, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube"}}); err != nil {
		t.Errorf("with the token: %v", err)
	}
}
