//go:build !nok8s

package server

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/execserve"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_pod_exec.go serves the daemon-host `fleet kube-exec` shim's daemon
// mode: one command in a Kubernetes instance's pod, through the pod's agent
// session (which the pool verified the pod for before connecting it). It is
// local-socket only (localOnlyMethods), as IssueKubeExecCredential is: the
// shim runs on the daemon host, and a remote client runs the same commands
// through the streaming Exec RPC. The first frame carries PodExecStart; the
// stream then carries stdin, resizes and the command's output and exit like
// an Exec stream (shared execserve pumps).

// podExecer is the Kubernetes backend handle's side of PodExec.
type podExecer interface {
	OpenPodExec(ctx context.Context, cmd kubernetes.PodCommand) (backend.ExecStream, error)
}

// podExecerFor is the handle of inst that runs its pod commands; a var so
// tests stand in for the pool.
var podExecerFor = func(inst *fleet.Instance) (podExecer, bool) {
	e, ok := backendutil.NewForInstance(inst, false).(podExecer)
	return e, ok
}

// PodExec implements the local-only PodExec RPC. The daemon's shutdown stops
// accepting new ones first (shutdown.go) and ends the open ones with the
// rest of its streams.
func (s *service) PodExec(stream fleetgrpc.FleetService_PodExecServer) error {
	if err := s.shuttingDown(); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ps := first.GetPodStart()
	if ps == nil {
		return status.Error(codes.InvalidArgument, "first PodExec frame must carry pod_start")
	}
	if ps.GetClusterId() == "" || ps.GetPod() == "" {
		return status.Error(codes.InvalidArgument, "pod_start needs a cluster and a pod")
	}
	if len(ps.GetArgv()) == 0 {
		return status.Error(codes.InvalidArgument, "pod exec argv must not be empty")
	}
	inst, err := resolveKubePod(ps.GetClusterId(), ps.GetPod())
	if err != nil {
		return err
	}
	execer, ok := podExecerFor(inst)
	if !ok {
		return status.Error(codes.FailedPrecondition, "the instance's backend cannot run a pod command")
	}

	logArgs := []any{
		"cluster", ps.GetClusterId(), "pod", ps.GetPod(),
		"cmd", strings.Join(ps.GetArgv(), " "), "tty", ps.GetTty(),
	}
	began := time.Now()
	proc, err := execer.OpenPodExec(stream.Context(), kubernetes.PodCommand{
		Class: podExecClass(ps.GetClass()), User: ps.GetUser(), Cwd: ps.GetCwd(),
		Argv: ps.GetArgv(), Env: ps.GetEnv(), TTY: ps.GetTty(),
		Rows: uint16(ps.GetRows()), Cols: uint16(ps.GetCols()), Bulk: ps.GetBulk(),
	})
	if err != nil {
		flog.Error("pod exec", append(logArgs, "ms", flog.MillisSince(began), "err", err)...)
		return status.Error(podExecCode(err), err.Error())
	}
	code, errMsg, sendErr := execserve.Serve(stream, proc, ps.GetTty())
	logArgs = append(logArgs, "code", code, "ms", flog.MillisSince(began))
	if errMsg != nil {
		flog.Error("pod exec", append(logArgs, "err", *errMsg)...)
	} else {
		flog.Info("pod exec", logArgs...)
	}
	return sendErr
}

// podExecClass maps the shim's class word to an execas class (empty is exec).
func podExecClass(class string) execas.Class {
	c := execas.Class(class)
	if c == "" {
		return execas.ClassExec
	}
	return c
}

// podExecCode is the gRPC code for a command that would not start.
func podExecCode(err error) codes.Code {
	switch status.Code(err) {
	case codes.OK:
		return codes.Unavailable
	default:
		return status.Code(err)
	}
}

// resolveKubePod is the Kubernetes instance whose pod is pod on connection
// clusterID (its StatefulSet is pod without the -0 suffix).
func resolveKubePod(clusterID, pod string) (*fleet.Instance, error) {
	sts := strings.TrimSuffix(pod, "-0")
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Backend != fleet.BackendKubernetes || inst.KubeCluster != clusterID || inst.KubeKey == "" {
				continue
			}
			if names.Instance(fleetName, inst.Name, inst.KubeKey) == sts {
				return inst, nil
			}
		}
	}
	return nil, status.Errorf(codes.NotFound, "no Kubernetes instance for pod %s on %s", pod, clusterID)
}
