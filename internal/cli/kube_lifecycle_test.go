package cli

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// recordingService is the one RPC a lifecycle job opens.
type recordingService struct {
	fleetgrpc.FleetServiceClient
	started, stopped []string
}

func (r *recordingService) StartInstance(_ context.Context, in *fleetgrpc.StartInstanceRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
	r.started = append(r.started, in.GetFleet()+"/"+in.GetInstance())
	return emptyStream{}, nil
}

func (r *recordingService) StopInstance(_ context.Context, in *fleetgrpc.StopInstanceRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
	r.stopped = append(r.stopped, in.GetFleet()+"/"+in.GetInstance())
	return emptyStream{}, nil
}

type emptyStream struct{ grpc.ClientStream }

func (emptyStream) Recv() (*fleetgrpc.JobEvent, error) { return nil, io.EOF }
func (emptyStream) Header() (metadata.MD, error)       { return nil, nil }

func seedLifecycleInstances(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st := &state.State{Fleets: map[string]*fleet.Fleet{"web": {
		Name: "web",
		Instances: []*fleet.Instance{
			{Name: "pod", Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc234", KubeKey: "abcd2345", ContainerID: "k8s:mk-abc234/ns/fi-web-pod-abcd2345@g1", Status: fleet.StatusStopped, CreatedAt: time.Unix(0, 0)},
			{Name: "up", Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc234", KubeKey: "bbbb2345", ContainerID: "k8s:mk-abc234/ns/fi-web-up-bbbb2345@g1", Status: fleet.StatusRunning, CreatedAt: time.Unix(0, 0)},
		},
	}}}
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}
}

// TestKubernetesStartAndStopRunInTheDaemon: `fleet start` and `fleet stop`
// of a Kubernetes instance go to the daemon (its StartInstance and
// StopInstance jobs), and one already where it is asked to be makes no call.
func TestKubernetesStartAndStopRunInTheDaemon(t *testing.T) {
	seedLifecycleInstances(t)
	svc := &recordingService{}
	orig := runInstanceJob
	t.Cleanup(func() { runInstanceJob = orig })
	runInstanceJob = func(ctx context.Context, _ bool, open func(context.Context, fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error)) error {
		_, err := open(ctx, svc)
		return err
	}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; _ = devnull.Close() })

	for _, tc := range []struct {
		cmd func() *cobra.Command
		ref string
	}{
		{newStartCmd, "web/pod"},
		{newStopCmd, "web/up"},
		// Already running / already stopped: nothing to ask.
		{newStartCmd, "web/up"},
		{newStopCmd, "web/pod"},
	} {
		c := tc.cmd()
		c.SetArgs([]string{tc.ref})
		if err := c.Execute(); err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
	}
	if len(svc.started) != 1 || svc.started[0] != "web/pod" || len(svc.stopped) != 1 || svc.stopped[0] != "web/up" {
		t.Errorf("started %v, stopped %v; want [web/pod] and [web/up]", svc.started, svc.stopped)
	}
}
