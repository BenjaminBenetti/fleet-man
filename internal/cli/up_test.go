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
