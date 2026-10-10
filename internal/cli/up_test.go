package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
)

// fakeJobStream replays a fixed event list.
type fakeJobStream struct {
	grpc.ClientStream
	events []*fleetgrpc.JobEvent
}

func (s *fakeJobStream) Recv() (*fleetgrpc.JobEvent, error) {
	if len(s.events) == 0 {
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

// fakeCreateClient records the CreateInstance request and answers it with a
// JobStarted carrying target (nil: a daemon that predates the field) and a
// successful JobDone.
type fakeCreateClient struct {
	fleetgrpc.FleetServiceClient
	target *fleetgrpc.DeployTarget
	got    *fleetgrpc.CreateInstanceRequest
}

func (c *fakeCreateClient) CreateInstance(_ context.Context, req *fleetgrpc.CreateInstanceRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
	c.got = req
	return &fakeJobStream{events: []*fleetgrpc.JobEvent{
		{Event: &fleetgrpc.JobEvent_Started{Started: &fleetgrpc.JobStarted{Target: c.target}}},
		{Event: &fleetgrpc.JobEvent_Done{Done: &fleetgrpc.JobDone{Success: true}}},
	}}, nil
}

// runUp runs `fleet up` against a fake daemon, draining the job stream the way
// runInstanceJob does, and returns what it printed.
func runUp(t *testing.T, client *fakeCreateClient, args ...string) (string, error) {
	t.Helper()
	return runUpWith(t, client, args...)
}

func TestUpWithoutBackendLeavesTheTargetToTheDaemon(t *testing.T) {
	if def := newUpCmd().Flags().Lookup("backend").DefValue; def != "" {
		t.Fatalf("--backend defaults to %q; it must be empty so fleetd's remembered target applies", def)
	}

	client := &fakeCreateClient{target: &fleetgrpc.DeployTarget{Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES}}
	out, err := runUp(t, client, "alpha/i1")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := client.got.GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED {
		t.Fatalf("request backend = %v, want UNSPECIFIED", got)
	}
	if client.got.GetRememberDefault() {
		t.Fatal("fleet up must not move the remembered default")
	}
	if !strings.Contains(out, "Creating alpha/i1 (backend: codespaces)...") {
		t.Fatalf("output should name the target fleetd resolved:\n%s", out)
	}
}

func TestUpWithBackendSendsIt(t *testing.T) {
	client := &fakeCreateClient{target: &fleetgrpc.DeployTarget{Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODER}}
	out, err := runUp(t, client, "alpha/i1", "--backend", "coder")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := client.got.GetBackend(); got != fleetgrpc.BackendType_BACKEND_TYPE_CODER {
		t.Fatalf("request backend = %v, want CODER", got)
	}
	if !strings.Contains(out, "(backend: coder)") {
		t.Fatalf("output = %q", out)
	}

	if _, err := runUp(t, &fakeCreateClient{}, "alpha/i1", "--backend", "bogus"); err == nil || !strings.Contains(err.Error(), "invalid backend") {
		t.Fatalf("--backend bogus: err = %v, want invalid backend", err)
	}
}

// A daemon that predates JobStarted.target: the line falls back to the flag,
// and without one says the daemon chose.
func TestUpAgainstADaemonWithoutJobStartedTarget(t *testing.T) {
	out, err := runUp(t, &fakeCreateClient{}, "alpha/i1", "--backend", "devcontainer")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if !strings.Contains(out, "(backend: devcontainer)") {
		t.Fatalf("output = %q", out)
	}
	out, err = runUp(t, &fakeCreateClient{}, "alpha/i1")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if !strings.Contains(out, "(backend: fleetd default)") {
		t.Fatalf("output = %q", out)
	}
}

// clusterCreateClient also answers ListKubeClusters.
type clusterCreateClient struct {
	*fakeCreateClient
	clusters []*fleetgrpc.KubeClusterStatus
}

func (c *clusterCreateClient) ListKubeClusters(context.Context, *fleetgrpc.ListKubeClustersRequest, ...grpc.CallOption) (*fleetgrpc.ListKubeClustersReply, error) {
	return &fleetgrpc.ListKubeClustersReply{Clusters: c.clusters}, nil
}

// TestUpOnACluster: --cluster names a connection by id, name or context and
// runs the instance on Kubernetes there; a non-Kubernetes --backend beside
// it is refused, as is a connection the daemon does not have.
func TestUpOnACluster(t *testing.T) {
	clusters := []*fleetgrpc.KubeClusterStatus{
		{Cluster: &fleetgrpc.KubeCluster{Id: "minikube-abc234", DisplayName: "laptop", Context: "minikube"}},
		{Cluster: &fleetgrpc.KubeCluster{Id: "prod-eu-xyz234", DisplayName: "prod", Context: "arn:aws:eks:eu-west-1:1:cluster/prod"}},
	}
	for _, ref := range []string{"prod-eu-xyz234", "prod", "arn:aws:eks:eu-west-1:1:cluster/prod"} {
		client := &clusterCreateClient{fakeCreateClient: &fakeCreateClient{}, clusters: clusters}
		if _, err := runUpWith(t, client, "alpha/i1", "--cluster", ref); err != nil {
			t.Fatalf("--cluster %s: %v", ref, err)
		}
		if got := client.got; got.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES || got.GetKubeCluster() != "prod-eu-xyz234" {
			t.Errorf("--cluster %s sent backend %v cluster %q", ref, got.GetBackend(), got.GetKubeCluster())
		}
	}
	client := &clusterCreateClient{fakeCreateClient: &fakeCreateClient{}, clusters: clusters}
	if _, err := runUpWith(t, client, "alpha/i1", "--cluster", "laptop", "--backend", "kubernetes"); err != nil || client.got.GetKubeCluster() != "minikube-abc234" {
		t.Errorf("--cluster with --backend kubernetes: %v, %v", err, client.got)
	}
	if _, err := runUpWith(t, &clusterCreateClient{fakeCreateClient: &fakeCreateClient{}, clusters: clusters}, "alpha/i1", "--cluster", "prod", "--backend", "coder"); err == nil {
		t.Error("--cluster with --backend coder was accepted")
	}
	if _, err := runUpWith(t, &clusterCreateClient{fakeCreateClient: &fakeCreateClient{}, clusters: clusters}, "alpha/i1", "--cluster", "nope"); err == nil {
		t.Error("--cluster naming no connection was accepted")
	}
}

// runUpWith is runUp against any fake service.
func runUpWith(t *testing.T, client fleetgrpc.FleetServiceClient, args ...string) (string, error) {
	t.Helper()
	origJob, origState := runInstanceJob, fetchFleetState
	t.Cleanup(func() { runInstanceJob, fetchFleetState = origJob, origState })
	fetchFleetState = func(context.Context) (*fleetgrpc.State, error) {
		return &fleetgrpc.State{Fleets: map[string]*fleetgrpc.Fleet{"alpha": {Name: "alpha", Remote: "git@x:alpha.git"}}}, nil
	}
	runInstanceJob = func(ctx context.Context, _ bool, open func(context.Context, fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error)) error {
		stream, err := open(ctx, client)
		if err != nil {
			return err
		}
		for {
			if _, err := stream.Recv(); err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}
		}
	}
	var out bytes.Buffer
	cmd := newUpCmd()
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
