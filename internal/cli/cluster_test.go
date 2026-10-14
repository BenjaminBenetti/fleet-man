package cli

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// fakeStream replays events, then io.EOF (or err).
type fakeStream[T any] struct {
	grpc.ClientStream
	events []*T
	err    error
}

func (s *fakeStream[T]) Recv() (*T, error) {
	if len(s.events) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

// fakeKube is a daemon's kube RPCs: canned replies, and every request kept.
type fakeKube struct {
	fleetgrpc.FleetServiceClient
	clusters    []*fleetgrpc.KubeClusterStatus
	contexts    *fleetgrpc.ListKubeContextsReply
	removeReply *fleetgrpc.RemoveKubeClusterReply
	testEvents  []*fleetgrpc.TestKubeClusterEvent
	testErr     error // the test stream's error after its events
	pruneReply  *fleetgrpc.PruneKubeClusterReply
	loginEvents []*fleetgrpc.LoginKubeClusterEvent
	nsReply     *fleetgrpc.CreateKubeNamespaceReply
	nsErr       error
	namespace   *fleetgrpc.KubeNamespaceResult // what Add and Update say fleetd did about it
	err         error                          // every RPC's error, when set

	calls    []string
	requests []proto.Message
}

func (f *fakeKube) record(name string, req proto.Message) error {
	f.calls = append(f.calls, name)
	f.requests = append(f.requests, req)
	return f.err
}

func (f *fakeKube) ListKubeContexts(_ context.Context, req *fleetgrpc.ListKubeContextsRequest, _ ...grpc.CallOption) (*fleetgrpc.ListKubeContextsReply, error) {
	return f.contexts, f.record("ListKubeContexts", req)
}

func (f *fakeKube) ListKubeClusters(_ context.Context, req *fleetgrpc.ListKubeClustersRequest, _ ...grpc.CallOption) (*fleetgrpc.ListKubeClustersReply, error) {
	return &fleetgrpc.ListKubeClustersReply{Clusters: f.clusters}, f.record("ListKubeClusters", req)
}

func (f *fakeKube) AddKubeCluster(_ context.Context, req *fleetgrpc.AddKubeClusterRequest, _ ...grpc.CallOption) (*fleetgrpc.AddKubeClusterReply, error) {
	c := proto.Clone(req.GetCluster()).(*fleetgrpc.KubeCluster)
	c.Id = c.GetContext() + "-a1b2c3"
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	return &fleetgrpc.AddKubeClusterReply{Cluster: &fleetgrpc.KubeClusterStatus{Cluster: c, State: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING}, Namespace: f.namespace},
		f.record("AddKubeCluster", req)
}

func (f *fakeKube) UpdateKubeCluster(_ context.Context, req *fleetgrpc.UpdateKubeClusterRequest, _ ...grpc.CallOption) (*fleetgrpc.UpdateKubeClusterReply, error) {
	st, _ := fleetgrpc.FindKubeCluster(f.clusters, req.GetId())
	return &fleetgrpc.UpdateKubeClusterReply{Cluster: st, Namespace: f.namespace}, f.record("UpdateKubeCluster", req)
}

func (f *fakeKube) RemoveKubeCluster(_ context.Context, req *fleetgrpc.RemoveKubeClusterRequest, _ ...grpc.CallOption) (*fleetgrpc.RemoveKubeClusterReply, error) {
	reply := f.removeReply
	if reply == nil {
		reply = &fleetgrpc.RemoveKubeClusterReply{Removed: true}
	}
	return reply, f.record("RemoveKubeCluster", req)
}

func (f *fakeKube) TestKubeCluster(_ context.Context, req *fleetgrpc.TestKubeClusterRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fleetgrpc.TestKubeClusterEvent], error) {
	return &fakeStream[fleetgrpc.TestKubeClusterEvent]{events: f.testEvents, err: f.testErr}, f.record("TestKubeCluster", req)
}

func (f *fakeKube) PruneKubeCluster(_ context.Context, req *fleetgrpc.PruneKubeClusterRequest, _ ...grpc.CallOption) (*fleetgrpc.PruneKubeClusterReply, error) {
	return f.pruneReply, f.record("PruneKubeCluster", req)
}

func (f *fakeKube) LoginKubeCluster(_ context.Context, req *fleetgrpc.LoginKubeClusterRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fleetgrpc.LoginKubeClusterEvent], error) {
	return &fakeStream[fleetgrpc.LoginKubeClusterEvent]{events: f.loginEvents}, f.record("LoginKubeCluster", req)
}

func (f *fakeKube) CreateKubeNamespace(_ context.Context, req *fleetgrpc.CreateKubeNamespaceRequest, _ ...grpc.CallOption) (*fleetgrpc.CreateKubeNamespaceReply, error) {
	if err := f.record("CreateKubeNamespace", req); err != nil {
		return nil, err
	}
	if req.GetDryRun() {
		return &fleetgrpc.CreateKubeNamespaceReply{Manifest: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: fleet\n"}, nil
	}
	return f.nsReply, f.nsErr
}

// last is the newest request of the named RPC.
func (f *fakeKube) last(t *testing.T, name string) proto.Message {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i] == name {
			return f.requests[i]
		}
	}
	t.Fatalf("no %s call; calls %v", name, f.calls)
	return nil
}

func kubeStatus(id, name, context, namespace string, state fleetgrpc.KubeClusterState, msg string) *fleetgrpc.KubeClusterStatus {
	return &fleetgrpc.KubeClusterStatus{
		Cluster: &fleetgrpc.KubeCluster{Id: id, DisplayName: name, Context: context, Namespace: namespace, KubeconfigPaths: []string{"/home/me/.kube/config"}},
		State:   state, Message: msg,
	}
}

func newFakeKube() *fakeKube {
	return &fakeKube{clusters: []*fleetgrpc.KubeClusterStatus{
		kubeStatus("minikube-a1b2c3", "minikube", "minikube", "default", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, ""),
		kubeStatus("prod-d4e5f6", "prod-eu", "gke_acme_europe_prod", "fleet", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, "privileged denied · each fleet on one node"),
	}}
}

// runClusterCLI runs `fleet <args>` against the fake daemon; stdout and
// stderr go to separate buffers.
func runClusterCLI(t *testing.T, f *fakeKube, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	orig := dialCluster
	t.Cleanup(func() { dialCluster = orig })
	dialCluster = func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) { return f, func() {}, nil }
	cmd := NewRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestClusterLs(t *testing.T) {
	f := newFakeKube()
	out, _, err := runClusterCLI(t, f, "cluster", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NAME      ID               CONTEXT               NAMESPACE  STATE    DETAIL",
		"minikube  minikube-a1b2c3  minikube              default    ready",
		"prod-eu   prod-d4e5f6      gke_acme_europe_prod  fleet      limited  privileged denied · each fleet on one node",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output lacks %q:\n%s", want, out)
		}
	}

	f.clusters[1].Warnings = []string{"privileged allowed"}
	f.clusters[1].Cluster.AllowPrivileged = ptrTo(true)
	out, _, err = runClusterCLI(t, f, "cluster", "list", "prod-eu")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "minikube-a1b2c3") {
		t.Errorf("ls <cluster> lists the others:\n%s", out)
	}
	for _, want := range []string{"warning: privileged allowed\n", "\nid=prod-d4e5f6\n", "\nallow_privileged=true\n", "\nkubeconfig_paths=/home/me/.kube/config\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls <cluster> lacks %q:\n%s", want, out)
		}
	}

	out, _, err = runClusterCLI(t, f, "cluster", "ls", "--json", "minikube")
	if err != nil {
		t.Fatal(err)
	}
	var parsed fleetgrpc.KubeClusters
	if err := protojson.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("ls --json is not KubeClusters JSON: %v\n%s", err, out)
	}
	if len(parsed.GetClusters()) != 1 || !proto.Equal(parsed.GetClusters()[0], f.clusters[0]) {
		t.Errorf("ls --json minikube = %v", parsed.GetClusters())
	}
	if !strings.Contains(out, "\n  \"clusters\": [\n") {
		t.Errorf("ls --json is not stably indented:\n%s", out)
	}

	out, _, err = runClusterCLI(t, &fakeKube{}, "cluster", "ls")
	if err != nil || !strings.Contains(out, "No cluster connections") {
		t.Errorf("empty ls = %q, %v", out, err)
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "ls", "nope"); err == nil || err.Error() != `no Kubernetes connection "nope"` {
		t.Errorf("ls nope = %v", err)
	}
}

// `ls <cluster>` of a remote daemon's connection that mirrors the daemon
// host's ports says whose ports those are; a local daemon's, or one with
// host_gateway off, does not.
func TestClusterLsWarnsOfARemoteDaemonsHostGateway(t *testing.T) {
	for _, k := range []string{fleetclient.EnvGateway, fleetclient.EnvSSH, fleetclient.EnvServer} {
		t.Setenv(k, "")
	}
	f := newFakeKube()
	out, _, err := runClusterCLI(t, f, "cluster", "ls", "prod-eu")
	if err != nil || strings.Contains(out, fleet.HostGatewayRemoteWarning) {
		t.Fatalf("a local daemon: %v\n%s", err, out)
	}
	t.Setenv(fleetclient.EnvSSH, "ssh://me@daemon-host")
	out, _, err = runClusterCLI(t, f, "cluster", "ls", "prod-eu")
	if err != nil || !strings.Contains(out, "warning: "+fleet.HostGatewayRemoteWarning+"\n") {
		t.Fatalf("a remote daemon: %v\n%s", err, out)
	}
	f.clusters[1].Cluster.HostGateway = "off"
	out, _, err = runClusterCLI(t, f, "cluster", "ls", "prod-eu")
	if err != nil || strings.Contains(out, fleet.HostGatewayRemoteWarning) {
		t.Fatalf("host_gateway off: %v\n%s", err, out)
	}
}

func TestClusterContexts(t *testing.T) {
	f := newFakeKube()
	f.contexts = &fleetgrpc.ListKubeContextsReply{
		KubeconfigPaths: []string{"/home/me/.kube/config"}, Host: "devbox",
		Contexts: []*fleetgrpc.KubeContext{
			{Name: "minikube", Cluster: "minikube", Namespace: "", Auth: "client-cert", Current: true, Connected: []string{"minikube-a1b2c3"}},
			{Name: "gke_acme_europe_prod", Cluster: "gke_acme", Namespace: "team", Auth: "exec-plugin", Plugin: "gke-gcloud-auth-plugin", PluginMissing: "not found"},
		},
	}
	// A path is the daemon host's: it goes as typed, never expanded here.
	out, errOut, err := runClusterCLI(t, f, "cluster", "contexts", "--kubeconfig", "~/.kube/config", "--kubeconfig", "rel/config")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.last(t, "ListKubeContexts").(*fleetgrpc.ListKubeContextsRequest).GetKubeconfigPaths(); strings.Join(got, ",") != "~/.kube/config,rel/config" {
		t.Errorf("kubeconfig paths sent = %v", got)
	}
	if errOut != "/home/me/.kube/config on fleetd @ devbox\n" {
		t.Errorf("stderr = %q", errOut)
	}
	for _, want := range []string{
		"CURRENT  NAME                  CLUSTER   NAMESPACE  AUTH                                          CONNECTED",
		"*        minikube              minikube             client-cert                                   minikube\n",
		"         gke_acme_europe_prod  gke_acme  team       exec-plugin:gke-gcloud-auth-plugin (missing)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("contexts output lacks %q:\n%s", want, out)
		}
	}
}

func TestClusterAdd(t *testing.T) {
	f := newFakeKube()
	out, _, err := runClusterCLI(t, f, "cluster", "add", "kind-dev", "--kubeconfig", "/srv/kube/config", "-n", "fleet",
		"allow_privileged=true", "extra_path=/opt/bin", "image.mode=node-load")
	if err != nil {
		t.Fatal(err)
	}
	got := f.last(t, "AddKubeCluster").(*fleetgrpc.AddKubeClusterRequest).GetCluster()
	want := &fleetgrpc.KubeCluster{DisplayName: "kind-dev", Context: "kind-dev", KubeconfigPaths: []string{"/srv/kube/config"}, Namespace: "fleet",
		AllowPrivileged: ptrTo(true), ExtraPath: []string{"/opt/bin"}, Image: &fleetgrpc.KubeImageSettings{Mode: "node-load"}}
	if !proto.Equal(got, want) {
		t.Errorf("added %v, want %v", got, want)
	}
	if out != "Added cluster \"kind-dev\" (kind-dev-a1b2c3): context kind-dev, namespace fleet; checking\n" {
		t.Errorf("output = %q", out)
	}

	// The context's name is taken: the namespace tells them apart; with
	// no namespace given, the daemon's id names it.
	if _, _, err := runClusterCLI(t, f, "cluster", "add", "minikube", "-n", "team"); err != nil {
		t.Fatal(err)
	}
	if name := f.last(t, "AddKubeCluster").(*fleetgrpc.AddKubeClusterRequest).GetCluster().GetDisplayName(); name != "minikube-team" {
		t.Errorf("default name of a second minikube = %q", name)
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "add", "MINIKUBE"); err != nil {
		t.Fatal(err)
	}
	if c := f.last(t, "AddKubeCluster").(*fleetgrpc.AddKubeClusterRequest).GetCluster(); c.GetDisplayName() != "" || len(c.GetKubeconfigPaths()) != 0 || c.GetNamespace() != "" {
		t.Errorf("defaults left to the daemon: %v", c)
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "add", "minikube", "--name", "lab"); err != nil {
		t.Fatal(err)
	}
	if name := f.last(t, "AddKubeCluster").(*fleetgrpc.AddKubeClusterRequest).GetCluster().GetDisplayName(); name != "lab" {
		t.Errorf("--name = %q", name)
	}
	// The daemon's display-name rules: at most 63 bytes, no control
	// character (C1 included).
	for _, unfit := range []string{strings.Repeat("x", 64), "dev\u0085"} {
		if _, _, err := runClusterCLI(t, f, "cluster", "add", unfit); err != nil {
			t.Fatal(err)
		}
		if name := f.last(t, "AddKubeCluster").(*fleetgrpc.AddKubeClusterRequest).GetCluster().GetDisplayName(); name != "" {
			t.Errorf("context %q, unfit for a name, became %q", unfit, name)
		}
	}

	for _, tc := range []struct {
		args []string
		err  string
	}{
		{[]string{"cluster", "add", "c", "-n", "a", "namespace=b"}, "not both"},
		{[]string{"cluster", "add", "c", "--kubeconfig", "/a", "kubeconfig_paths=/b"}, "not both"},
		{[]string{"cluster", "add", "c", "context=d"}, "give the context once"},
		{[]string{"cluster", "add", "c", "--namespace-label", "nope"}, "not key=value"},
		{[]string{"cluster", "add", "c", "id=x"}, "id is set by fleetd"},
	} {
		f := newFakeKube()
		if _, _, err := runClusterCLI(t, f, tc.args...); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%v = %v, want %q", tc.args, err, tc.err)
		}
		for _, c := range f.calls {
			if c == "AddKubeCluster" {
				t.Errorf("%v added a connection", tc.args)
			}
		}
	}
}

// fleetd creates a missing namespace as it adds the connection, with the
// labels given; the output says so, or why not, with the manifest to apply
// when it may not. The connection is added either way.
func TestClusterAddReportsTheNamespace(t *testing.T) {
	f := newFakeKube()
	f.namespace = &fleetgrpc.KubeNamespaceResult{Namespace: "fleet-base", Created: true}
	out, _, err := runClusterCLI(t, f, "cluster", "add", "kind-dev", "-n", "fleet-base",
		"--namespace-label", "pod-security.kubernetes.io/enforce=baseline")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, ",") != "ListKubeClusters,AddKubeCluster" {
		t.Errorf("calls = %v", f.calls)
	}
	req := f.last(t, "AddKubeCluster").(*fleetgrpc.AddKubeClusterRequest)
	if req.GetCluster().GetNamespace() != "fleet-base" || len(req.GetNamespaceLabels()) != 1 || req.GetNamespaceLabels()["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Errorf("add request = %v", req)
	}
	if want := "Added cluster \"kind-dev\" (kind-dev-a1b2c3): context kind-dev, namespace fleet-base; checking\nCreated namespace fleet-base\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	// One that exists says nothing more.
	f = newFakeKube()
	f.namespace = &fleetgrpc.KubeNamespaceResult{Namespace: "fleet"}
	out, _, err = runClusterCLI(t, f, "cluster", "add", "kind-dev", "-n", "fleet")
	if err != nil || strings.Count(out, "\n") != 1 {
		t.Errorf("existing namespace: %q, %v", out, err)
	}

	// fleetd may not create it: the connection is still added, and the
	// manifest is printed for someone who may.
	f = newFakeKube()
	f.namespace = &fleetgrpc.KubeNamespaceResult{Namespace: "fleet", PermissionDenied: true,
		Error:    `creating namespace "fleet": forbidden (it needs namespaces create; apply the manifest as someone who has it)`,
		Manifest: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: fleet\n"}
	out, _, err = runClusterCLI(t, f, "cluster", "add", "kind-dev", "-n", "fleet")
	if err != nil {
		t.Fatalf("a refused namespace must not fail the add: %v", err)
	}
	want := "Added cluster \"kind-dev\" (kind-dev-a1b2c3): context kind-dev, namespace fleet; checking\n" +
		"warning: namespace fleet does not exist: creating namespace \"fleet\": forbidden (it needs namespaces create; apply the manifest as someone who has it)\n" +
		"Namespace manifest:\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: fleet\n"
	if out != want {
		t.Errorf("refused namespace output =\n%q\nwant\n%q", out, want)
	}

	// A cluster fleetd did not reach: it could not tell.
	f = newFakeKube()
	f.namespace = &fleetgrpc.KubeNamespaceResult{Namespace: "fleet", Error: "could not tell whether it exists: connection refused"}
	out, _, _ = runClusterCLI(t, f, "cluster", "add", "kind-dev", "-n", "fleet")
	if !strings.HasSuffix(out, "warning: namespace fleet: could not tell whether it exists: connection refused\n") {
		t.Errorf("unreached output = %q", out)
	}
}

// Setting a namespace says what fleetd did about it too.
func TestClusterSetReportsTheNamespace(t *testing.T) {
	f := newFakeKube()
	f.namespace = &fleetgrpc.KubeNamespaceResult{Namespace: "team-a", Created: true}
	out, _, err := runClusterCLI(t, f, "cluster", "set", "minikube", "namespace=team-a")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Updated cluster \"minikube\": namespace\nCreated namespace team-a\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestClusterSet(t *testing.T) {
	f := newFakeKube()
	out, _, err := runClusterCLI(t, f, "cluster", "set", "prod-eu", "allow_privileged=true", "extra_path=/opt/a", "extra_path=/opt/b",
		"resources.cpu_request=2", "storage_class=")
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t, "UpdateKubeCluster").(*fleetgrpc.UpdateKubeClusterRequest)
	if req.GetId() != "prod-d4e5f6" || req.GetExpectedRevision() != 0 {
		t.Errorf("update %v", req)
	}
	if got := strings.Join(req.GetUpdateMask().GetPaths(), ","); got != "allow_privileged,extra_path,resources.cpu_request,storage_class" {
		t.Errorf("mask = %s", got)
	}
	want := &fleetgrpc.KubeCluster{AllowPrivileged: ptrTo(true), ExtraPath: []string{"/opt/a", "/opt/b"}, Resources: &fleetgrpc.KubeResources{CpuRequest: "2"}}
	if !proto.Equal(req.GetCluster(), want) {
		t.Errorf("values = %v, want %v", req.GetCluster(), want)
	}
	if out != "Updated cluster \"prod-eu\": allow_privileged, extra_path, resources.cpu_request, storage_class\n" {
		t.Errorf("output = %q", out)
	}
	// By id and by context too.
	for _, ref := range []string{"prod-d4e5f6", "gke_acme_europe_prod"} {
		if _, _, err := runClusterCLI(t, f, "cluster", "set", ref, "max_instances=3"); err != nil {
			t.Fatal(err)
		}
		if id := f.last(t, "UpdateKubeCluster").(*fleetgrpc.UpdateKubeClusterRequest).GetId(); id != "prod-d4e5f6" {
			t.Errorf("set %s updated %s", ref, id)
		}
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "set", "prod-eu", "nope=1"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown field = %v", err)
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "set", "prod-eu"); err == nil {
		t.Error("set with no fields succeeded")
	}
	f.err = status.Error(codes.Aborted, `connection "prod-eu" changed since it was read`)
	if _, _, err := runClusterCLI(t, f, "cluster", "set", "prod-eu", "max_instances=3"); err == nil || err.Error() != `connection "prod-eu" changed since it was read` {
		t.Errorf("RPC error = %v", err)
	}
}

func TestClusterRemove(t *testing.T) {
	f := newFakeKube()
	out, _, err := runClusterCLI(t, f, "cluster", "rm", "minikube")
	if err != nil || out != "Removed cluster \"minikube\"\n" {
		t.Fatalf("rm = %q, %v", out, err)
	}
	if req := f.last(t, "RemoveKubeCluster").(*fleetgrpc.RemoveKubeClusterRequest); req.GetId() != "minikube-a1b2c3" || req.GetMode() != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_REFUSE_IF_USED {
		t.Errorf("remove %v", req)
	}

	users := []*fleetgrpc.KubeClusterUser{
		{Fleet: "alpha", Instance: "i1", Status: fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING},
		{Fleet: "alpha", Agent: "builder"},
	}
	f.removeReply = &fleetgrpc.RemoveKubeClusterReply{Users: users}
	_, _, err = runClusterCLI(t, f, "cluster", "remove", "minikube")
	if err == nil || !strings.Contains(err.Error(), "cluster \"minikube\" is in use:\n  alpha/i1 (running)\n  alpha: agent builder\nuse --destroy-instances") {
		t.Errorf("refused rm = %v", err)
	}

	// Forget names what it leaves behind: the instances in the namespace and
	// the agents that still target the connection.
	f.removeReply = &fleetgrpc.RemoveKubeClusterReply{Removed: true, Users: users, Warnings: []string{"the instances were left running"}}
	out, _, err = runClusterCLI(t, f, "cluster", "rm", "minikube", "--forget")
	if err != nil {
		t.Fatal(err)
	}
	if req := f.last(t, "RemoveKubeCluster").(*fleetgrpc.RemoveKubeClusterRequest); req.GetMode() != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET {
		t.Errorf("--forget sent %v", req.GetMode())
	}
	if out != "warning: the instances were left running\nRemoved cluster \"minikube\"\nleft in namespace default:\n  alpha/i1\n"+
		"agents left without a cluster:\n  alpha: agent builder\n" {
		t.Errorf("forget output = %q", out)
	}
	f.removeReply = &fleetgrpc.RemoveKubeClusterReply{Removed: true, Users: users[:1]}
	out, _, err = runClusterCLI(t, f, "cluster", "rm", "minikube", "--destroy-instances")
	if err != nil {
		t.Fatal(err)
	}
	if req := f.last(t, "RemoveKubeCluster").(*fleetgrpc.RemoveKubeClusterRequest); req.GetMode() != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES {
		t.Errorf("--destroy-instances sent %v", req.GetMode())
	}
	if !strings.HasSuffix(out, "Removed cluster \"minikube\"\ndestroyed:\n  alpha/i1\n") {
		t.Errorf("destroy output = %q", out)
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "rm", "minikube", "--forget", "--destroy-instances"); err == nil || !strings.Contains(err.Error(), "exclude each other") {
		t.Errorf("both modes = %v", err)
	}
}

func checkEvent(name string, result fleetgrpc.KubeCheckResult, detail, fix string) *fleetgrpc.TestKubeClusterEvent {
	return &fleetgrpc.TestKubeClusterEvent{Kind: &fleetgrpc.TestKubeClusterEvent_Check{Check: &fleetgrpc.KubeCheck{Name: name, Result: result, Detail: detail, Fix: fix}}}
}

func resultEvent(st *fleetgrpc.KubeClusterStatus) *fleetgrpc.TestKubeClusterEvent {
	return &fleetgrpc.TestKubeClusterEvent{Kind: &fleetgrpc.TestKubeClusterEvent_Result{Result: st}}
}

func TestClusterTest(t *testing.T) {
	f := newFakeKube()
	limited := proto.Clone(f.clusters[1]).(*fleetgrpc.KubeClusterStatus)
	limited.Warnings = []string{"privileged allowed"}
	f.testEvents = []*fleetgrpc.TestKubeClusterEvent{
		checkEvent("kubeconfig", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING, "", ""),
		checkEvent("kubeconfig", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, "~/.kube/config: gke_acme_europe_prod", ""),
		checkEvent("admission_unprivileged", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, "", ""),
		checkEvent("admission_privileged", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, "violates PodSecurity \"baseline:latest\"", "label the namespace privileged"),
		checkEvent("limits", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, "LimitRange default memory 1Gi\n→ explicit 8Gi limit", ""),
		resultEvent(limited),
	}
	out, _, err := runClusterCLI(t, f, "cluster", "test", "prod-eu")
	if err != nil {
		t.Fatalf("a limited cluster's test failed: %v", err)
	}
	if req := f.last(t, "TestKubeCluster").(*fleetgrpc.TestKubeClusterRequest); req.GetId() != "prod-d4e5f6" || req.GetCluster() != nil || req.GetDeep() {
		t.Errorf("test %v", req)
	}
	want := `CHECK                   RESULT  DETAIL
kubeconfig              pass    ~/.kube/config: gke_acme_europe_prod
admission unprivileged  pass
admission privileged    fail    violates PodSecurity "baseline:latest"
                                fix: label the namespace privileged
limits                  warn    LimitRange default memory 1Gi
                                → explicit 8Gi limit
warning: privileged allowed
limited: privileged denied · each fleet on one node
`
	if out != want {
		t.Errorf("test output:\n%s\nwant:\n%s", out, want)
	}

	// Anything but ready or limited exits 1, after the result line only.
	f.testEvents = []*fleetgrpc.TestKubeClusterEvent{
		checkEvent("reachable", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, "dial tcp 10.0.0.1:443: i/o timeout", ""),
		resultEvent(kubeStatus("prod-d4e5f6", "prod-eu", "gke", "fleet", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE, "the API server does not answer")),
	}
	out, errOut, err := runClusterCLI(t, f, "cluster", "test", "prod-eu")
	if err == nil {
		t.Fatal("an unreachable cluster's test passed")
	}
	if !strings.HasSuffix(out, "unreachable: the API server does not answer\n") || errOut != "" {
		t.Errorf("failed test: out %q, err %q", out, errOut)
	}

	f.testEvents = nil
	if _, _, err := runClusterCLI(t, f, "cluster", "test", "prod-eu"); err == nil || !strings.Contains(err.Error(), "without a result") {
		t.Errorf("no result = %v", err)
	}

	// --deep asks the daemon for the throwaway-pod probes too.
	f.testEvents = []*fleetgrpc.TestKubeClusterEvent{
		checkEvent("image_pull", fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, "a pod pulled the probe image through gw.example.com", ""),
		resultEvent(limited),
	}
	if _, _, err := runClusterCLI(t, f, "cluster", "test", "--deep", "prod-eu"); err != nil {
		t.Fatalf("deep test: %v", err)
	}
	if req := f.last(t, "TestKubeCluster").(*fleetgrpc.TestKubeClusterRequest); req.GetId() != "prod-d4e5f6" || !req.GetDeep() {
		t.Errorf("deep test %v", req)
	}

	// --storage names the StorageClasses to test across two nodes now.
	f.testEvents = []*fleetgrpc.TestKubeClusterEvent{resultEvent(limited)}
	if _, _, err := runClusterCLI(t, f, "cluster", "test", "--storage", "filestore", "--storage", "nfs", "prod-eu"); err != nil {
		t.Fatalf("storage test: %v", err)
	}
	if req := f.last(t, "TestKubeCluster").(*fleetgrpc.TestKubeClusterRequest); req.GetDeep() || !slices.Equal(req.GetStorageClasses(), []string{"filestore", "nfs"}) {
		t.Errorf("storage test %v", req)
	}
}

func TestClusterTestDraft(t *testing.T) {
	f := newFakeKube()
	f.testEvents = []*fleetgrpc.TestKubeClusterEvent{resultEvent(kubeStatus("", "", "kind-dev", "fleet", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, ""))}
	out, _, err := runClusterCLI(t, f, "cluster", "test", "--context", "kind-dev", "--kubeconfig", "/srv/kube/config", "-n", "fleet", "allow_privileged=false")
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t, "TestKubeCluster").(*fleetgrpc.TestKubeClusterRequest)
	want := &fleetgrpc.KubeCluster{Context: "kind-dev", KubeconfigPaths: []string{"/srv/kube/config"}, Namespace: "fleet", AllowPrivileged: ptrTo(false)}
	if req.GetId() != "" || !proto.Equal(req.GetCluster(), want) {
		t.Errorf("draft test %v", req)
	}
	if !strings.HasSuffix(out, "\nready\n") {
		t.Errorf("output %q", out)
	}
	// A draft the daemon refuses (a streamed RPC fails on its first
	// receive) prints the error alone, no table.
	f.testEvents, f.testErr = nil, status.Error(codes.NotFound, `no context "nope" in [/srv/kube/config] on the daemon host`)
	out, _, err = runClusterCLI(t, f, "cluster", "test", "--context", "nope", "--kubeconfig", "/srv/kube/config")
	if err == nil || err.Error() != `no context "nope" in [/srv/kube/config] on the daemon host` || out != "" {
		t.Errorf("refused draft: out %q, err %v", out, err)
	}
	f.testErr = nil
	for _, args := range [][]string{
		{"cluster", "test", "prod-eu", "--kubeconfig", "/a"},
		{"cluster", "test", "prod-eu", "-n", "x"},
		{"cluster", "test", "--context", "c", "notafield"},
		{"cluster", "test", "--context", "c", "--deep"},
		{"cluster", "test", "--context", "c", "--storage", "nfs"},
		{"cluster", "test"},
		{"cluster", "test", "a", "b"},
	} {
		if _, _, err := runClusterCLI(t, f, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

func TestClusterGC(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(now.Add(-d)) }
	f := newFakeKube()
	f.pruneReply = &fleetgrpc.PruneKubeClusterReply{Owner: "3fa2c91e0b77", LeaseHeld: true, Objects: []*fleetgrpc.KubePruneObject{
		{Kind: "sts", Name: "fi-fleet-man-old-1a2b3c4d", CreatedAt: ago(9 * 24 * time.Hour), Action: "would delete"},
		{Kind: "pvc", Name: "fi-fleet-man-old-1a2b3c4d", CreatedAt: ago(9 * 24 * time.Hour), Action: "would delete"},
		{Kind: "pvc", Name: "fi-fleet-man-new-5e6f7a8b", CreatedAt: ago(20 * time.Minute), Mounted: true, Action: "skip (younger than 1h)"},
	}}
	out, _, err := runClusterCLI(t, f, "cluster", "gc", "minikube")
	if err != nil {
		t.Fatal(err)
	}
	if req := f.last(t, "PruneKubeCluster").(*fleetgrpc.PruneKubeClusterRequest); req.GetId() != "minikube-a1b2c3" || req.GetApply() || req.GetFleet() != "" {
		t.Errorf("gc %v", req)
	}
	want := `owner 3fa2c91e0b77 · lease held by this daemon
KIND  NAME                       AGE  MOUNTED  ACTION
sts   fi-fleet-man-old-1a2b3c4d  9d   -        would delete
pvc   fi-fleet-man-old-1a2b3c4d  9d   no       would delete
pvc   fi-fleet-man-new-5e6f7a8b  20m  yes      skip (younger than 1h)
3 objects; run with --apply to delete
`
	if out != want {
		t.Errorf("gc output:\n%s\nwant:\n%s", out, want)
	}

	f.pruneReply = &fleetgrpc.PruneKubeClusterReply{Owner: "3fa2c91e0b77", LeaseHeld: true, Deleted: 1, Objects: []*fleetgrpc.KubePruneObject{
		{Kind: "pvc", Name: "fs-alpha-h8-share", Action: "deleted"},
	}}
	out, _, err = runClusterCLI(t, f, "cluster", "gc", "minikube", "--apply", "--fleet", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if req := f.last(t, "PruneKubeCluster").(*fleetgrpc.PruneKubeClusterRequest); !req.GetApply() || req.GetFleet() != "alpha" {
		t.Errorf("gc --apply %v", req)
	}
	if !strings.Contains(out, "pvc   fs-alpha-h8-share  -    no       deleted\n1 deleted\n") {
		t.Errorf("apply output:\n%s", out)
	}

	f.pruneReply = &fleetgrpc.PruneKubeClusterReply{Owner: "3fa2c91e0b77", LeaseHolder: "9e8d7c6b5a49-0011223344556677", LeaseHolderHost: "laptop",
		Objects: []*fleetgrpc.KubePruneObject{{Kind: "sts", Name: "x", Action: "would delete"}}}
	out, _, _ = runClusterCLI(t, f, "cluster", "gc", "minikube")
	if !strings.HasPrefix(out, "owner 3fa2c91e0b77 · lease held by 9e8d7c6b5a49-0011223344556677 on laptop\n") ||
		!strings.HasSuffix(out, "1 object; --apply is refused while another daemon holds the owner Lease\n") {
		t.Errorf("conflict output:\n%s", out)
	}

	f.pruneReply = &fleetgrpc.PruneKubeClusterReply{Owner: "3fa2c91e0b77", LeaseHeld: true}
	out, _, _ = runClusterCLI(t, f, "cluster", "gc", "minikube")
	if out != "owner 3fa2c91e0b77 · lease held by this daemon\nno objects to collect\n" {
		t.Errorf("empty gc = %q", out)
	}
	f.err = status.Error(codes.FailedPrecondition, "owner_conflict: another daemon holds the Lease")
	if _, _, err := runClusterCLI(t, f, "cluster", "gc", "minikube", "--apply"); err == nil || err.Error() != "owner_conflict: another daemon holds the Lease" {
		t.Errorf("refused apply = %v", err)
	}
}

func TestShortAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 45 * time.Second: "45s", 20 * time.Minute: "20m", 3 * time.Hour: "3h", 47 * time.Hour: "47h", 9 * 24 * time.Hour: "9d",
	} {
		if got := shortAge(d); got != want {
			t.Errorf("shortAge(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestClusterLogin(t *testing.T) {
	f := newFakeKube()
	f.loginEvents = []*fleetgrpc.LoginKubeClusterEvent{
		{Kind: &fleetgrpc.LoginKubeClusterEvent_DeviceCode{DeviceCode: &fleetgrpc.KubeDeviceCode{Url: "https://microsoft.com/devicelogin", Code: "ABCD-EFGH"}}},
		{Kind: &fleetgrpc.LoginKubeClusterEvent_Result{Result: f.clusters[1]}},
	}
	out, _, err := runClusterCLI(t, f, "cluster", "login", "prod-eu")
	if err != nil {
		t.Fatal(err)
	}
	if req := f.last(t, "LoginKubeCluster").(*fleetgrpc.LoginKubeClusterRequest); req.GetId() != "prod-d4e5f6" {
		t.Errorf("login %v", req)
	}
	want := "To log in, open https://microsoft.com/devicelogin and enter the code ABCD-EFGH\n" +
		"Logged in; cluster \"prod-eu\" is limited: privileged denied · each fleet on one node\n"
	if out != want {
		t.Errorf("login output %q", out)
	}
	f.loginEvents = nil
	if _, _, err := runClusterCLI(t, f, "cluster", "login", "prod-eu"); err == nil {
		t.Error("a login without a result succeeded")
	}
}

func TestClusterErrors(t *testing.T) {
	f := newFakeKube()
	f.err = status.Error(codes.Unimplemented, "unknown method ListKubeClusters")
	out, errOut, err := runClusterCLI(t, f, "cluster", "ls")
	if err == nil || err.Error() != "this fleetd has no Kubernetes support (update it)" {
		t.Errorf("old daemon = %v", err)
	}
	// A failed RPC prints its error, not the usage.
	if strings.Contains(out+errOut, "Usage:") {
		t.Errorf("RPC failure printed the usage:\n%s%s", out, errOut)
	}
	// An argument mistake does print it.
	out, errOut, _ = runClusterCLI(t, newFakeKube(), "cluster", "rm")
	if !strings.Contains(out+errOut, "Usage:") {
		t.Errorf("an argument error printed no usage:\n%s%s", out, errOut)
	}
}

// Every command and flag sends the same requests whatever daemon the CLI
// points at: nothing on the client side depends on where fleetd runs, so a
// remote daemon (gateway, Armada SSH, FLEET_SERVER) gets exactly what a
// local one does.
func TestClusterCommandsAreTheSameForRemoteDaemons(t *testing.T) {
	scripts := [][]string{
		{"cluster", "ls"},
		{"cluster", "ls", "prod-eu", "--json"},
		{"cluster", "contexts", "--kubeconfig", "/srv/kube/config"},
		{"cluster", "add", "kind-dev", "--name", "dev", "--kubeconfig", "/srv/kube/config", "-n", "fleet",
			"--namespace-label", "a=b", "extra_path=/opt/bin", "plugin_env=AWS_PROFILE", "allow_privileged=true", "pod_overrides_yaml=hostNetwork: true"},
		append([]string{"cluster", "set", "prod-eu"}, clusterFieldLines(everyClusterField())...),
		{"cluster", "test", "prod-eu"},
		{"cluster", "test", "--context", "kind-dev", "--kubeconfig", "/srv/kube/config", "-n", "fleet", "extra_path=/opt/bin"},
		{"cluster", "gc", "minikube", "--apply", "--fleet", "alpha"},
		{"cluster", "rm", "minikube", "--forget"},
		{"cluster", "rm", "prod-eu", "--destroy-instances"},
		{"cluster", "login", "prod-eu"},
	}
	run := func(env map[string]string) [][]proto.Message {
		for _, k := range []string{fleetclient.EnvGateway, fleetclient.EnvSSH, fleetclient.EnvServer, fleetclient.EnvToken} {
			t.Setenv(k, env[k])
		}
		var all [][]proto.Message
		for _, args := range scripts {
			f := newFakeKube()
			f.testEvents = []*fleetgrpc.TestKubeClusterEvent{resultEvent(f.clusters[0])}
			f.pruneReply = &fleetgrpc.PruneKubeClusterReply{LeaseHeld: true}
			f.loginEvents = []*fleetgrpc.LoginKubeClusterEvent{{Kind: &fleetgrpc.LoginKubeClusterEvent_Result{Result: f.clusters[1]}}}
			f.nsReply = &fleetgrpc.CreateKubeNamespaceReply{Created: true}
			if _, _, err := runClusterCLI(t, f, args...); err != nil {
				t.Fatalf("%v with %v: %v", args, env, err)
			}
			all = append(all, f.requests)
		}
		return all
	}
	local := run(nil)
	for _, env := range []map[string]string{
		{fleetclient.EnvGateway: "https://gw.example.com/grpc/abc", fleetclient.EnvToken: "t"},
		{fleetclient.EnvSSH: "ssh://me@daemon-host"},
		{fleetclient.EnvServer: "daemon-host:7000"},
	} {
		remote := run(env)
		for i := range scripts {
			if len(remote[i]) != len(local[i]) {
				t.Fatalf("%v sent %d requests remotely (%v), %d locally", scripts[i], len(remote[i]), env, len(local[i]))
			}
			for j := range local[i] {
				if !proto.Equal(remote[i][j], local[i][j]) {
					t.Errorf("%v with %v sent %v, locally %v", scripts[i], env, remote[i][j], local[i][j])
				}
			}
		}
	}
	// The set script carries every settable field: a remote daemon is sent
	// all of them.
	f := newFakeKube()
	if _, _, err := runClusterCLI(t, f, scripts[4]...); err != nil {
		t.Fatal(err)
	}
	req := f.last(t, "UpdateKubeCluster").(*fleetgrpc.UpdateKubeClusterRequest)
	if !proto.Equal(req.GetCluster(), everyClusterField()) {
		t.Errorf("set of every field sent %v", req.GetCluster())
	}
}

// `set --help` lists the fields (built when help is shown), and so does
// `fleet help cluster set`.
func TestClusterSetHelpListsTheFields(t *testing.T) {
	for _, args := range [][]string{{"cluster", "set", "--help"}, {"help", "cluster", "set"}} {
		out, _, err := runClusterCLI(t, newFakeKube(), args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Fields (kube.proto):", " allow_privileged ", " image.upload_pace_percent ", "Usage:"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v lacks %q:\n%s", args, want, out)
			}
		}
	}
}
