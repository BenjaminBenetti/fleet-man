package cli

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"google.golang.org/grpc"
)

// startedHookStream wraps a job stream to run onStarted once, on its
// JobStarted (the first event, sent once the daemon has accepted the job).
type startedHookStream struct {
	grpc.ServerStreamingClient[fleetgrpc.JobEvent]
	onStarted func(*fleetgrpc.JobStarted)
}

// onJobStarted returns stream with fn run on its JobStarted.
func onJobStarted(stream grpc.ServerStreamingClient[fleetgrpc.JobEvent], fn func(*fleetgrpc.JobStarted)) grpc.ServerStreamingClient[fleetgrpc.JobEvent] {
	return &startedHookStream{ServerStreamingClient: stream, onStarted: fn}
}

func (s *startedHookStream) Recv() (*fleetgrpc.JobEvent, error) {
	ev, err := s.ServerStreamingClient.Recv()
	if started := ev.GetStarted(); started != nil && s.onStarted != nil {
		fn := s.onStarted
		s.onStarted = nil
		fn(started)
	}
	return ev, err
}

// startedTargetLabel names the target a create job resolved to. A daemon that
// predates JobStarted.target leaves it unset: then the --backend the user
// passed is the target, and without one the daemon picked its default.
func startedTargetLabel(started *fleetgrpc.JobStarted, backendFlag string) string {
	if target := protoconv.DeployTargetFromProto(started.GetTarget()); !target.IsZero() {
		return target.String()
	}
	if backendFlag != "" {
		return backendFlag
	}
	return "fleetd default"
}
