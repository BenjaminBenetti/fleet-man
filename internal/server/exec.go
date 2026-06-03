package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exec.go implements the interactive-backend RPCs that let clients reach a
// container without importing internal/backend directly (the P5 boundary).
//
// ResolveExecCommand is the TTY carve-out (exec.proto): the server returns the
// argv it WOULD run (built by the backend), and the LOCAL client execs it itself
// so the user's terminal is inherited. Exec (bidi) — for a remote client with no
// local backend — is not implemented yet.

// resolveServerInstance loads the persisted record for fleet/instance, mapping
// absence to a clear NotFound (so a typo'd target fails fast for the client).
func resolveServerInstance(fleetName, instanceName string) (*fleet.Instance, error) {
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "instance %q not found in fleet %q", instanceName, fleetName)
	}
	return inst, nil
}

// ResolveExecCommand returns the argv (+ env) the server would exec to run argv
// inside the instance. Exec/ExecCommand/ExecCommandQuiet all build from the same
// rawExec, so ExecCommand's argv is exactly what an interactive `Exec` would run
// — the client execs it locally with inherited stdio.
func (s *service) ResolveExecCommand(_ context.Context, req *fleetgrpc.ResolveExecCommandRequest) (*fleetgrpc.ResolveExecCommandReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	cmd := backendutil.NewForInstance(inst, false).ExecCommand(inst.WorkspaceDir, req.GetArgv()).Cmd
	return &fleetgrpc.ResolveExecCommandReply{
		Argv: cmd.Args,
		Env:  envSliceToMap(cmd.Env),
	}, nil
}

// envSliceToMap converts a "K=V" env slice to the proto map. A nil slice (the
// common case — the command inherits the process environment) yields nil, so a
// local client that runs the argv likewise inherits its own environment.
func envSliceToMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}
