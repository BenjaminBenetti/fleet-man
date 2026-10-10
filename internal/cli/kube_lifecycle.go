package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"google.golang.org/grpc"
)

// kube_lifecycle.go runs `fleet start` and `fleet stop` of a Kubernetes
// instance in the daemon, which holds the cluster connections and the
// instance pods' agent sessions: a start in the CLI's own process would
// connect agents that die with it, and only the daemon delivers an image
// again. The other backends keep starting and stopping in the CLI process.

// lifecycleInDaemon is a start (or stop) of a Kubernetes instance through
// the daemon. handled is false for any other instance, which the caller
// starts or stops itself.
func lifecycleInDaemon(ctx context.Context, target *fleet.Target, start bool) (handled bool, err error) {
	_, _, inst, err := instanceops.LoadInstance(target.Fleet, target.Instance)
	if err != nil || inst.Backend != fleet.BackendKubernetes {
		return false, nil
	}
	want, verb, doing := fleet.StatusStopped, "stopped", "Stopping"
	if start {
		want, verb, doing = fleet.StatusRunning, "running", "Starting"
	}
	if inst.Status == want {
		fmt.Printf("Instance %s/%s is already %s.\n", target.Fleet, target.Instance, verb)
		return true, nil
	}
	fmt.Printf("%s %s/%s...\n", doing, target.Fleet, target.Instance)
	err = runInstanceJob(ctx, false, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
		if start {
			return svc.StartInstance(ctx, &fleetgrpc.StartInstanceRequest{Fleet: target.Fleet, Instance: target.Instance})
		}
		return svc.StopInstance(ctx, &fleetgrpc.StopInstanceRequest{Fleet: target.Fleet, Instance: target.Instance})
	})
	if err != nil {
		return true, err
	}
	if start {
		fmt.Printf("Instance %s/%s started.\n", target.Fleet, target.Instance)
	} else {
		fmt.Printf("Instance %s/%s stopped.\n", target.Fleet, target.Instance)
	}
	return true, nil
}
